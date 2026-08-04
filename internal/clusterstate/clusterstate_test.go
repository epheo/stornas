package clusterstate

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

func stateWith(t *testing.T, pools []*storagev1alpha1.StoragePool, nodes []*corev1.Node, pvcs []*corev1.PersistentVolumeClaim) *State {
	t.Helper()
	s := &State{pools: newIndexer(), shares: newIndexer(), nodes: newIndexer(), pvcs: newIndexer()}
	for _, p := range pools {
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: obj}
		u.SetName(p.Name)
		if err := s.pools.Add(u); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range nodes {
		if err := s.nodes.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	for _, pvc := range pvcs {
		if err := s.pvcs.Add(pvc); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestSnapshotPoolMapping(t *testing.T) {
	cap100 := resource.MustParse("100")
	free90 := resource.MustParse("90")
	pool := &storagev1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tank"},
		Spec: storagev1alpha1.StoragePoolSpec{
			Node:    "node-a",
			Raid:    "raid1",
			Devices: []string{"/dev/sda", "/dev/sdb"},
		},
		Status: storagev1alpha1.StoragePoolStatus{
			VG:          "stornas-tank",
			LinstorPool: "stornas",
			Capacity:    &cap100,
			Free:        &free90,
			Health:      "Online",
			Devices: []storagev1alpha1.DeviceStatus{
				{Path: "/dev/sda", State: "InSync", Smart: "Passed"},
			},
			Conditions: []metav1.Condition{{
				Type:               storagev1alpha1.ConditionAvailable,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ReasonReady,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}

	snap := stateWith(t, []*storagev1alpha1.StoragePool{pool}, nil, nil).Snapshot()
	if len(snap.Pools) != 1 {
		t.Fatalf("pools = %+v", snap.Pools)
	}
	p := snap.Pools[0]
	if p.Name != "tank" || p.Node != "node-a" || !p.Available || p.Reason != "Ready" {
		t.Fatalf("pool = %+v", p)
	}
	if p.CapacityBytes != 100 || p.FreeBytes != 90 || p.Health != "Online" {
		t.Fatalf("pool = %+v", p)
	}
	// Spec order preserved; the unreported device is listed with empty state.
	if len(p.Devices) != 2 || p.Devices[0].State != "InSync" || p.Devices[1].Path != "/dev/sdb" || p.Devices[1].State != "" {
		t.Fatalf("devices = %+v", p.Devices)
	}
}

func TestSnapshotPoolWithoutStatus(t *testing.T) {
	pool := &storagev1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh"},
		Spec:       storagev1alpha1.StoragePoolSpec{Node: "node-a", Devices: []string{"/dev/sda"}},
	}
	snap := stateWith(t, []*storagev1alpha1.StoragePool{pool}, nil, nil).Snapshot()
	p := snap.Pools[0]
	if p.Health != "Unknown" || p.Available {
		t.Fatalf("pool = %+v", p)
	}
}

func TestSnapshotNodeAndVolume(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.1.10"}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.35.0"},
		},
	}
	block := corev1.PersistentVolumeBlock
	sc := "stornas-replicated"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "vms", Name: "disk-0"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			VolumeMode:       &block,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	snap := stateWith(t, nil, []*corev1.Node{node}, []*corev1.PersistentVolumeClaim{pvc}).Snapshot()
	n := snap.Nodes[0]
	if !n.Ready || len(n.Roles) != 1 || n.Roles[0] != "control-plane" || n.Addresses[0] != "192.168.1.10" {
		t.Fatalf("node = %+v", n)
	}
	v := snap.Volumes[0]
	if v.StorageClass != "stornas-replicated" || !v.Block || v.Phase != "Bound" || v.CapacityBytes != 10*1024*1024*1024 {
		t.Fatalf("volume = %+v", v)
	}
}
