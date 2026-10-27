package agent

import (
	"context"
	"slices"
	"strings"
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

// A status echo must not rewrite the passdb: smbpasswd re-derives NT
// hashes per user per node. A rotated secret must.
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
		t.Fatalf("status echo rewrote the passdb: %v", f.calls[converged:])
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

// A deleted or emptied password Secret must revoke the user, once per
// change, without touching other users; recreating it must restore them.
func TestUserAgentRevokesOnSecretLoss(t *testing.T) {
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
	secret := func(pw string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "alice-pw", Namespace: "stornas-system"},
			Data:       map[string][]byte{"password": []byte(pw)},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(user, secret("hunter2")).Build()

	f := &fakeRunner{results: map[string]result{
		"id -u alice":                               {out: "1001"},
		"smbpasswd -s -a alice":                     {},
		"smbpasswd -x alice":                        {},
		"smbstatus --processes --user alice --json": {out: `{"sessions": {}}`},
	}}
	r := &UserAgentReconciler{Client: c, Secrets: c, Run: f}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "stornas-system", Name: "alice"}}
	ctx := context.Background()
	reconcile := func() []string {
		t.Helper()
		f.calls = nil
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		return f.calls
	}

	reconcile()
	if err := c.Delete(ctx, secret("")); err != nil {
		t.Fatal(err)
	}
	if calls := reconcile(); !slices.Contains(calls, "smbpasswd -x alice") {
		t.Fatalf("deleted Secret left the passdb entry: %v", calls)
	}
	if calls := reconcile(); len(calls) != 0 {
		t.Fatalf("status echo repeated the revocation: %v", calls)
	}

	if err := c.Create(ctx, secret("rotated")); err != nil {
		t.Fatal(err)
	}
	if calls := reconcile(); !slices.Contains(calls, "smbpasswd -s -a alice") {
		t.Fatalf("recreated Secret never restored the user: %v", calls)
	}
	if f.stdins["smbpasswd -s -a alice"] != "rotated\nrotated\n" {
		t.Fatalf("stdin = %q", f.stdins["smbpasswd -s -a alice"])
	}

	emptied := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "stornas-system", Name: "alice-pw"}, emptied); err != nil {
		t.Fatal(err)
	}
	delete(emptied.Data, "password")
	if err := c.Update(ctx, emptied); err != nil {
		t.Fatal(err)
	}
	calls := reconcile()
	if !slices.Contains(calls, "smbpasswd -x alice") {
		t.Fatalf("emptied Secret left the passdb entry: %v", calls)
	}
	for _, call := range calls {
		if strings.Contains(call, "smbpasswd") && !strings.HasSuffix(call, " alice") {
			t.Fatalf("revocation touched another user: %s", call)
		}
	}
}
