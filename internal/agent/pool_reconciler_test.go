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
