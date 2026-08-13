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
