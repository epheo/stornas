package clusterstate

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"

	"github.com/epheo/stornas/internal/model"
)

func syncing(pct int32) *int32 { return &pct }

func TestReplicationModel(t *testing.T) {
	v := &rdrbdv1alpha1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Status: rdrbdv1alpha1.VolumeStatus{Replicas: []rdrbdv1alpha1.ReplicaStatus{
			{Node: "node3", DiskState: "Diskless", Peers: map[string]string{"node2": "Connected", "node1": "Connected"}},
			{Node: "node2", DiskState: "Inconsistent", SyncPercent: syncing(43), Peers: map[string]string{"node1": "Connected", "node3": "Connected"}},
			{Node: "node1", DiskState: "UpToDate", InUse: true, Peers: map[string]string{"node2": "Connected", "node3": "Connecting"}},
			{Node: "node4"},
		}},
	}
	got := replicationModel(v)
	pct := 43
	want := &model.Replication{Replicas: []model.Replica{
		{Node: "node1", DiskState: "UpToDate", InUse: true, Peers: []model.Peer{
			{Node: "node2", Connected: true, Status: "Connected"},
			{Node: "node3", Connected: false, Status: "Connecting"},
		}},
		{Node: "node2", DiskState: "Inconsistent", SyncPercent: &pct, Peers: []model.Peer{
			{Node: "node1", Connected: true, Status: "Connected"},
			{Node: "node3", Connected: true, Status: "Connected"},
		}},
		{Node: "node3", DiskState: "Diskless", Peers: []model.Peer{
			{Node: "node1", Connected: true, Status: "Connected"},
			{Node: "node2", Connected: true, Status: "Connected"},
		}},
		{Node: "node4", DiskState: "Unknown"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// Split brain is r-drbd's verdict, not a guess from connection strings: a
// StandAlone peer without the condition is not flagged.
func TestReplicationSplitBrainFollowsCondition(t *testing.T) {
	standalone := []rdrbdv1alpha1.ReplicaStatus{
		{Node: "a", DiskState: "UpToDate", Peers: map[string]string{"b": "StandAlone"}},
		{Node: "b", DiskState: "UpToDate", Peers: map[string]string{"a": "StandAlone"}},
	}
	v := &rdrbdv1alpha1.Volume{Status: rdrbdv1alpha1.VolumeStatus{Replicas: standalone}}
	if replicationModel(v).SplitBrain {
		t.Fatal("split brain without the condition")
	}
	v.Status.Conditions = []metav1.Condition{{Type: rdrbdv1alpha1.ConditionSplitBrain, Status: metav1.ConditionTrue}}
	if !replicationModel(v).SplitBrain {
		t.Fatal("split brain condition not reflected")
	}
}

// The admin fixes a firewall from the message, so it passes through whole.
func TestReplicationPeerUnreachableFollowsCondition(t *testing.T) {
	v := &rdrbdv1alpha1.Volume{}
	if replicationModel(v).PeerUnreachable != nil {
		t.Fatal("peer unreachable without the condition")
	}
	msg := "node-a cannot reach node-b at 192.0.2.2:11000"
	v.Status.Conditions = []metav1.Condition{{Type: rdrbdv1alpha1.ConditionPeerUnreachable, Status: metav1.ConditionFalse, Reason: "Connected"}}
	if replicationModel(v).PeerUnreachable != nil {
		t.Fatal("False condition reflected")
	}
	v.Status.Conditions[0] = metav1.Condition{Type: rdrbdv1alpha1.ConditionPeerUnreachable, Status: metav1.ConditionTrue, Reason: "Firewall", Message: msg}
	got := replicationModel(v).PeerUnreachable
	if got == nil || got.Reason != "Firewall" || got.Message != msg {
		t.Fatalf("got %+v", got)
	}
}

// Volumes join PVC rows by PV name, which is the Volume name.
func TestSnapshotDecoratesVolumes(t *testing.T) {
	s := stateWith(t, nil, nil, []*corev1.PersistentVolumeClaim{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bound"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pending"}},
	})
	v := &rdrbdv1alpha1.Volume{
		TypeMeta:   metav1.TypeMeta{APIVersion: "r-drbd.io/v1alpha1", Kind: "Volume"},
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Status:     rdrbdv1alpha1.VolumeStatus{Replicas: []rdrbdv1alpha1.ReplicaStatus{{Node: "node1", DiskState: "UpToDate"}}},
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.backing.Add(&unstructured.Unstructured{Object: obj}); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.Volumes[0].Name != "bound" || snap.Volumes[0].Replication == nil ||
		snap.Volumes[0].Replication.Replicas[0].DiskState != "UpToDate" {
		t.Fatalf("bound = %+v", snap.Volumes[0])
	}
	if snap.Volumes[1].Replication != nil {
		t.Fatalf("pending = %+v", snap.Volumes[1])
	}
}
