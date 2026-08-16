package agent

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// secretRefreshInterval bounds how long a rotated or late-created Secret
// goes unnoticed: nothing watches Secrets (get-only RBAC), so reconciles
// that read one poll instead.
const secretRefreshInterval = 5 * time.Minute

// UserAgentReconciler provisions SMB users into the host passdb on every
// node (shares can be placed anywhere). Secrets are read uncached through
// the API reader: the agent's RBAC only grants get in its own namespace,
// and a cached client would demand a cluster-wide list.
type UserAgentReconciler struct {
	client.Client
	Secrets client.Reader
	Run     Runner

	// applied remembers the secret version last written to the passdb so
	// quiet refresh ticks skip the smbpasswd rewrite (NT hash derivation
	// plus a passdb write per user per node, forever). In-memory: an
	// agent restart reconverges. Single controller goroutine, no lock.
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
		// The operator flags missing secrets on the LocalUser; poll until
		// the ref appears.
		return ctrl.Result{RequeueAfter: secretRefreshInterval}, client.IgnoreNotFound(err)
	}
	pw := string(secret.Data["password"])
	if pw == "" {
		return ctrl.Result{RequeueAfter: secretRefreshInterval}, nil
	}
	if r.applied[user.Name] == secret.ResourceVersion {
		// Converged and the secret has not rotated: the tick only had to
		// re-read it.
		return ctrl.Result{RequeueAfter: secretRefreshInterval}, nil
	}
	if err := EnsureSMBUser(ctx, r.Run, user.Name, pw); err != nil {
		return ctrl.Result{}, err
	}
	if r.applied == nil {
		r.applied = map[string]string{}
	}
	r.applied[user.Name] = secret.ResourceVersion
	return ctrl.Result{RequeueAfter: secretRefreshInterval}, nil
}

func (r *UserAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.LocalUser{}).
		Named("localuser-agent").
		Complete(r)
}
