package agent

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/lvm"
)

// A transient LVM failure must flip HostReady but keep the last-known
// device view; blanking it would hide the degraded disk it explains.
func TestPoolReconcilerKeepsDevicesOnTransientError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pool := &storagev1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tank", Namespace: "stornas-system"},
		Spec:       storagev1alpha1.StoragePoolSpec{Node: "node-a", Devices: []string{"/dev/sda"}},
		Status: storagev1alpha1.StoragePoolStatus{
			Devices: []storagev1alpha1.DeviceStatus{{Path: "/dev/sda", State: "InSync"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pool).WithStatusSubresource(pool).Build()

	// vgs fails: every later step short-circuits with an empty report.
	f := &fakeRunner{results: map[string]result{
		"vgs stornas-tank":  {err: errExit},
		"pvs /dev/sda":      {err: errExit},
		"pvcreate /dev/sda": {err: errExit},
	}}
	r := &PoolReconciler{Client: c, Node: "node-a", LVM: lvm.NewWithRunner(f), MD: mdOf(f)}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "stornas-system", Name: "tank"}}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("want the host error back for backoff")
	}

	got := &storagev1alpha1.StoragePool{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Devices) != 1 || got.Status.Devices[0].Path != "/dev/sda" {
		t.Fatalf("devices blanked: %+v", got.Status.Devices)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, storagev1alpha1.ConditionHostReady) {
		t.Fatal("HostReady must be false on a failed pass")
	}
}

// The wipe must wait for the operator's Deregistered clearance: before
// it, LINSTOR may still back live volumes with this VG, and a kubectl
// delete would otherwise destroy them.
func TestPoolReconcilerHoldsWipeUntilDeregistered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	pool := &storagev1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tank", Namespace: "stornas-system",
			DeletionTimestamp: &now,
			Finalizers:        []string{"storage.stornas.io/linstor-deregister"},
		},
		Spec: storagev1alpha1.StoragePoolSpec{Node: "node-a", Devices: []string{"/dev/vdb"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pool).WithStatusSubresource(pool).Build()

	// No stubs: any host command before clearance is the bug.
	f := &fakeRunner{results: map[string]result{}}
	r := &PoolReconciler{Client: c, Node: "node-a", LVM: lvm.NewWithRunner(f), MD: mdOf(f)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "stornas-system", Name: "tank"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("host touched before Deregistered: %v", f.calls)
	}

	got := &storagev1alpha1.StoragePool{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
		Type:   storagev1alpha1.ConditionDeregistered,
		Status: metav1.ConditionTrue,
		Reason: storagev1alpha1.ReasonReady,
	})
	if err := c.Status().Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}

	f.results = map[string]result{
		"vgremove -ff -y stornas-tank": {out: `Volume group "stornas-tank" not found`, err: errExit},
		"test -b /dev/vdb":             {err: errExit},
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, storagev1alpha1.ConditionTornDown) {
		t.Fatalf("TornDown not set after clearance: %+v", got.Status.Conditions)
	}
}
