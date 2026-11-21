package agent

import (
	"context"
	"log"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/epheo/stornas/internal/auth"
	"github.com/epheo/stornas/internal/console"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// agetty reads /run/issue.d only when /etc/issue is absent, and the image
// ships /etc/issue.
const (
	IssuePath = "/etc/issue.d/50-stornas.issue"
	MOTDPath  = "/run/motd.d/50-stornas"
)

// BannerReconciler writes this node's login screen and MOTD from the
// published certificate fingerprint and the users still on a generated
// password. The files cover every user, so all events share one request.
type BannerReconciler struct {
	client.Client
	// Secrets reads uncached: the agent may only get Secrets.
	Secrets   client.Reader
	Run       Runner
	Namespace string
	// Root prefixes the host paths; tests write to a temp tree.
	Root string
}

func (r *BannerReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	fingerprint := ""
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: console.ConfigMapName}, &cm)
	switch {
	case err == nil:
		fingerprint = cm.Data[console.KeyFingerprint]
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	var users storagev1alpha1.LocalUserList
	if err := r.List(ctx, &users, client.InNamespace(r.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	passwords := map[string]string{}
	for _, u := range users.Items {
		if u.Annotations[auth.AnnotGenerated] != "true" {
			continue
		}
		var s corev1.Secret
		err := r.Secrets.Get(ctx, types.NamespacedName{Namespace: u.Namespace, Name: u.Spec.PasswordSecretRef}, &s)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if pw, ok := storagev1alpha1.LocalUserPassword(s.Data); ok {
			passwords[u.Name] = pw
		}
	}
	issue, motd := console.Render(fingerprint, passwords)
	changed, err := writeIfChanged(filepath.Join(r.Root, IssuePath), issue)
	if err != nil {
		return ctrl.Result{}, err
	}
	if _, err := writeIfChanged(filepath.Join(r.Root, MOTDPath), motd); err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		// A getty already at its prompt shows the new text only after a
		// reload; the file is in place either way, so a failure only logs.
		if out, err := r.Run.Run(ctx, "agetty", "--reload"); err != nil {
			log.Printf("agetty --reload: %v: %s", err, out)
		}
	}
	return ctrl.Result{}, nil
}

// writeIfChanged keeps the files root-only: they may hold a password.
func writeIfChanged(path, content string) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

func (r *BannerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "banner"}}}
	})
	published := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == r.Namespace && o.GetName() == console.ConfigMapName
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("banner").
		Watches(&storagev1alpha1.LocalUser{}, one).
		Watches(&corev1.ConfigMap{}, one, builder.WithPredicates(published)).
		Complete(r)
}
