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
// and a cached client would demand a cluster-wide list. Only the operator
// watches Secrets; its status.passwordSecretVersion stamp is what wakes
// this reconcile on a rotation.
type UserAgentReconciler struct {
	client.Client
	Secrets client.Reader
	Run     Runner

	// applied remembers the secret version last written to the passdb so
	// the status writes that echo every change skip the smbpasswd rewrite
	// (NT hash derivation plus a passdb write per user per node). Keyed on
	// the live RV, not the stamp: the stamp can land after the LocalUser,
	// and keying on it would rewrite the passdb twice. In-memory: an agent
	// restart reconverges. Single controller goroutine, no lock.
	applied map[string]string
}

func (r *UserAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var user storagev1alpha1.LocalUser
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Deleted: a live passdb entry would keep share logins
			// working after the user is gone.
			delete(r.applied, req.Name)
			RemoveSMBUser(ctx, r.Run, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !user.Spec.SMB {
		// Also the smb:true -> false edge; absent entries are quiet.
		delete(r.applied, user.Name)
		RemoveSMBUser(ctx, r.Run, user.Name)
		return ctrl.Result{}, nil
	}
	var secret corev1.Secret
	if err := r.Secrets.Get(ctx, types.NamespacedName{Namespace: user.Namespace, Name: user.Spec.PasswordSecretRef}, &secret); err != nil {
		// The operator flags a missing Secret on the LocalUser, and its
		// version stamp wakes this reconcile once the Secret appears.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	pw := string(secret.Data["password"])
	if pw == "" {
		return ctrl.Result{}, nil
	}
	if r.applied[user.Name] == secret.ResourceVersion {
		return ctrl.Result{}, nil
	}
	if err := EnsureSMBUser(ctx, r.Run, user.Name, pw); err != nil {
		return ctrl.Result{}, err
	}
	if r.applied == nil {
		r.applied = map[string]string{}
	}
	r.applied[user.Name] = secret.ResourceVersion
	return ctrl.Result{}, nil
}

func (r *UserAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.LocalUser{}).
		Named("localuser-agent").
		Complete(r)
}
