package agent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// A target placed elsewhere must be torn down here, not ignored: after a
// failover this reconcile on the ex-primary is the fencing.
func TestTargetAgentTearsDownWhenPlacedElsewhere(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	target := &storagev1alpha1.Target{
		ObjectMeta: metav1.ObjectMeta{Name: "vms", Namespace: "stornas-system"},
		Spec:       storagev1alpha1.TargetSpec{VIP: "192.168.1.50/24"},
		Status: storagev1alpha1.TargetStatus{
			IQN:        "iqn.2026-08.io.stornas:vms",
			ActiveNode: "node-b",
			LUNs:       []storagev1alpha1.LUNStatus{{ID: 0, Device: "/dev/drbd1000"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(target).WithStatusSubresource(target).Build()

	iqn := "iqn.2026-08.io.stornas:vms"
	f := &fakeRunner{results: map[string]result{
		"targetcli ls /iscsi/" + iqn + " 1":                   {out: "o- vms\n"},
		"targetcli /iscsi delete " + iqn:                      {},
		"targetcli ls /backstores/block 1":                    {out: "o- block\n  o- stornas-vms-lun0 [x]\n"},
		"targetcli /backstores/block delete stornas-vms-lun0": {},
		"targetcli saveconfig":                                {},
		"ip -j addr show to 192.168.1.50":                     {out: `[{"ifname":"eth0"}]`},
		"ip addr del 192.168.1.50/24 dev eth0":                {},
	}}
	r := &TargetAgentReconciler{Client: c, Secrets: c, Node: "node-a", LIO: &LIOManager{Run: f}}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "stornas-system", Name: "vms"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	torn := false
	for _, call := range f.calls {
		if call == "targetcli /iscsi delete "+iqn {
			torn = true
		}
	}
	if !torn {
		t.Fatalf("export left on the losing node: %v", f.calls)
	}
}

// A deleting target must be torn down while the spec (the only place the
// VIP lives) still exists, then confirmed with State Removed so the
// operator releases its finalizer.
func TestTargetAgentRemovesOnDeletion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	target := &storagev1alpha1.Target{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vms", Namespace: "stornas-system",
			DeletionTimestamp: &now,
			Finalizers:        []string{"storage.stornas.io/teardown"},
		},
		Spec: storagev1alpha1.TargetSpec{VIP: "192.168.1.50/24"},
		Status: storagev1alpha1.TargetStatus{
			IQN:        "iqn.2026-08.io.stornas:vms",
			ActiveNode: "node-a",
			State:      "Exported",
			LUNs:       []storagev1alpha1.LUNStatus{{ID: 0, Device: "/dev/drbd1000"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(target).WithStatusSubresource(target).Build()

	iqn := "iqn.2026-08.io.stornas:vms"
	f := &fakeRunner{results: map[string]result{
		"targetcli ls /iscsi/" + iqn + " 1":                   {out: "o- vms\n"},
		"targetcli /iscsi delete " + iqn:                      {},
		"targetcli ls /backstores/block 1":                    {out: "o- block\n  o- stornas-vms-lun0 [x]\n"},
		"targetcli /backstores/block delete stornas-vms-lun0": {},
		"targetcli saveconfig":                                {},
		"ip -j addr show to 192.168.1.50":                     {out: `[{"ifname":"eth0"}]`},
		"ip addr del 192.168.1.50/24 dev eth0":                {},
	}}
	r := &TargetAgentReconciler{Client: c, Secrets: c, Node: "node-a", LIO: &LIOManager{Run: f}}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "stornas-system", Name: "vms"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	vipDropped := false
	for _, call := range f.calls {
		if call == "ip addr del 192.168.1.50/24 dev eth0" {
			vipDropped = true
		}
	}
	if !vipDropped {
		t.Fatalf("VIP left on the node: %v", f.calls)
	}
	got := &storagev1alpha1.Target{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.State != "Removed" {
		t.Fatalf("state = %q, teardown unconfirmed", got.Status.State)
	}
}

// A quiet refresh tick (same generation, placement, secrets) must skip
// the targetcli walk: it is a dozen ~1s python spawns per target that
// reconverge an already-converged state.
func TestTargetAgentSkipsWalkWhenUnchanged(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	iqn := "iqn.2026-08.io.stornas:vms"
	target := &storagev1alpha1.Target{
		ObjectMeta: metav1.ObjectMeta{Name: "vms", Namespace: "stornas-system"},
		Status: storagev1alpha1.TargetStatus{
			IQN:        iqn,
			ActiveNode: "node-a",
			LUNs:       []storagev1alpha1.LUNStatus{{ID: 0, Device: "/dev/drbd1000"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(target).WithStatusSubresource(target).Build()

	f := &fakeRunner{results: map[string]result{
		"targetcli /iscsi create " + iqn:                                                   {},
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000":       {},
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0": {},
		"targetcli /iscsi/" + iqn + "/tpg1 set attribute authentication=0":                 {},
		"targetcli ls /iscsi/" + iqn + "/tpg1/acls 1":                                      {},
		"targetcli ls /iscsi/" + iqn + "/tpg1/luns 1":                                      {},
		"targetcli saveconfig":                                                             {},
	}}
	r := &TargetAgentReconciler{Client: c, Secrets: c, Node: "node-a", LIO: &LIOManager{Run: f}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "stornas-system", Name: "vms"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	converged := len(f.calls)
	if converged == 0 {
		t.Fatal("first pass must converge")
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != converged {
		t.Fatalf("quiet tick reran the walk: %v", f.calls[converged:])
	}
}
