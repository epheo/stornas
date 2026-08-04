package agent

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// UserAgentReconciler provisions SMB users into the host passdb on every
// node (shares can be placed anywhere). Secrets are read uncached through
// the API reader: the agent's RBAC only grants get in its own namespace,
// and a cached client would demand a cluster-wide list.
type UserAgentReconciler struct {
	client.Client
	Secrets client.Reader
	Run     Runner
}

func (r *UserAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var user storagev1alpha1.LocalUser
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !user.Spec.SMB {
		return ctrl.Result{}, nil
	}
	var secret corev1.Secret
	if err := r.Secrets.Get(ctx, types.NamespacedName{Namespace: user.Namespace, Name: user.Spec.PasswordSecretRef}, &secret); err != nil {
		// The operator flags missing secrets on the LocalUser; nothing to do here.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	pw := string(secret.Data["password"])
	if pw == "" {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, EnsureSMBUser(ctx, r.Run, user.Name, pw)
}

func (r *UserAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.LocalUser{}).
		Named("localuser-agent").
		Complete(r)
}
