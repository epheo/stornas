package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// A quiet refresh tick must not rewrite the passdb: smbpasswd re-derives
// NT hashes per user per node forever. A rotated secret must.
func TestUserAgentSkipsPassdbWhenSecretUnchanged(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	user := &storagev1alpha1.LocalUser{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "stornas-system"},
		Spec:       storagev1alpha1.LocalUserSpec{SMB: true, PasswordSecretRef: "alice-pw"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-pw", Namespace: "stornas-system"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(user, secret).Build()

	f := &fakeRunner{results: map[string]result{
		"id -u alice":           {out: "1001"},
		"smbpasswd -s -a alice": {},
	}}
	r := &UserAgentReconciler{Client: c, Secrets: c, Run: f}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "stornas-system", Name: "alice"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	converged := len(f.calls)
	if converged == 0 {
		t.Fatal("first pass must provision")
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != converged {
		t.Fatalf("quiet tick rewrote the passdb: %v", f.calls[converged:])
	}

	// Rotation (a new resourceVersion) must reach the passdb.
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "stornas-system", Name: "alice-pw"}, got); err != nil {
		t.Fatal(err)
	}
	got.Data["password"] = []byte("rotated")
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) == converged {
		t.Fatal("rotated secret never reached the passdb")
	}
	if f.stdins["smbpasswd -s -a alice"] != "rotated\nrotated\n" {
		t.Fatalf("stdin = %q", f.stdins["smbpasswd -s -a alice"])
	}
}
