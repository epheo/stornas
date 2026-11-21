package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/epheo/stornas/internal/auth"
	"github.com/epheo/stornas/internal/console"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// The login screen shows the fingerprint and the generated password; once
// the admin changes it, the password leaves the screen and agetty reloads.
func TestBannerShowsGeneratedPasswordUntilChanged(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ns := "stornas-system"
	admin := &storagev1alpha1.LocalUser{
		ObjectMeta: metav1.ObjectMeta{Name: "admin", Namespace: ns, Annotations: map[string]string{auth.AnnotGenerated: "true"}},
		Spec:       storagev1alpha1.LocalUserSpec{PasswordSecretRef: "admin-password"},
	}
	objs := []client.Object{
		admin,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-password", Namespace: ns},
			Data:       map[string][]byte{"password": []byte("GENERATED42")},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: console.ConfigMapName, Namespace: ns},
			Data:       map[string]string{console.KeyFingerprint: "AB:CD"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	run := &fakeRunner{results: map[string]result{"agetty --reload": {}}}
	root := t.TempDir()
	r := &BannerReconciler{Client: c, Secrets: c, Run: run, Namespace: ns, Root: root}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	issue := readFile(t, filepath.Join(root, IssuePath))
	if !strings.Contains(issue, "AB:CD") || !strings.Contains(issue, "GENERATED42") {
		t.Fatalf("issue:\n%s", issue)
	}
	if info, err := os.Stat(filepath.Join(root, MOTDPath)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("motd mode: %v %v", info, err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("calls = %v", run.calls)
	}

	// Unchanged state: no rewrite, no reload.
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("reloaded without a change: %v", run.calls)
	}

	admin.Annotations = nil
	if err := c.Update(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{IssuePath, MOTDPath} {
		if got := readFile(t, filepath.Join(root, p)); strings.Contains(got, "GENERATED42") {
			t.Fatalf("%s still shows the password:\n%s", p, got)
		}
	}
	if len(run.calls) != 2 {
		t.Fatalf("calls = %v", run.calls)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
