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

// A deleting share must be confirmed with State Removed so the operator's
// finalizer releases; only the serving node confirms.
func TestShareReconcilerConfirmsTeardown(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	sh := &storagev1alpha1.Share{
		ObjectMeta: metav1.ObjectMeta{
			Name: "media", Namespace: "stornas-system",
			DeletionTimestamp: &now,
			Finalizers:        []string{"storage.stornas.io/teardown"},
		},
		Spec:   storagev1alpha1.ShareSpec{ClaimName: "media", NFS: &storagev1alpha1.NFSExport{Clients: []string{"*"}}},
		Status: storagev1alpha1.ShareStatus{Node: "node-a", Device: "/dev/drbd1000", State: "Exported"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(sh).WithStatusSubresource(sh).Build()

	// Nothing was built on disk (Root is an empty temp dir), so no host
	// commands run; the confirmation must still land.
	m := &ShareManager{Run: &fakeRunner{results: map[string]result{}}, Node: "node-a", Root: t.TempDir()}
	r := &ShareAgentReconciler{Client: c, Node: "node-a", Shares: m}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "stornas-system", Name: "media"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	got := &storagev1alpha1.Share{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.State != "Removed" {
		t.Fatalf("state = %q, want Removed", got.Status.State)
	}

	// A standby node must not write status for a share it never served.
	standby := &ShareAgentReconciler{Client: c, Node: "node-b", Shares: &ShareManager{
		Run: &fakeRunner{results: map[string]result{}}, Node: "node-b", Root: t.TempDir()}}
	got.Status.State = "Exported"
	if err := c.Status().Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := standby.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.State != "Exported" {
		t.Fatalf("standby overwrote state to %q", got.Status.State)
	}
}
