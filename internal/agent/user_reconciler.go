package agent

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// applied remembers what this node last made the passdb reflect (a
	// Secret version, or revoked) so the status writes that echo every
	// change skip the smbpasswd rewrite (NT hash derivation plus a passdb
	// write per user per node). Keyed on the live RV, not the stamp: the
	// stamp can land after the LocalUser, and keying on it would rewrite
	// the passdb twice. In-memory: an agent restart reconverges. Single
	// controller goroutine, no lock.
	applied map[string]string
}

// revoked is the applied key of a user the passdb must not hold;
// resourceVersions are numeric, so it never collides with one.
const revoked = "revoked"

func (r *UserAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var user storagev1alpha1.LocalUser
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		// Deleted: a live passdb entry would keep share logins
		// working after the user is gone.
		delete(r.applied, req.Name)
		return ctrl.Result{}, RemoveSMBUser(ctx, r.Run, req.Name)
	}
	key, password := revoked, ""
	if user.Spec.SMB {
		var secret corev1.Secret
		err := r.Secrets.Get(ctx, types.NamespacedName{Namespace: user.Namespace, Name: user.Spec.PasswordSecretRef}, &secret)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// A missing or empty Secret revokes like smb off: a passdb entry
		// left behind would keep the old password working. The operator
		// flags it on the LocalUser, and its version stamp wakes this
		// reconcile once the Secret is usable again.
		if pw, ok := storagev1alpha1.LocalUserPassword(secret.Data); ok {
			key, password = secret.ResourceVersion, pw
		}
	}
	if r.applied[user.Name] == key {
		return ctrl.Result{}, nil
	}
	var err error
	if key == revoked {
		err = RemoveSMBUser(ctx, r.Run, user.Name)
	} else {
		err = EnsureSMBUser(ctx, r.Run, user.Name, password)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if r.applied == nil {
		r.applied = map[string]string{}
	}
	r.applied[user.Name] = key
	return ctrl.Result{}, nil
}

func (r *UserAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.LocalUser{}).
		Named("localuser-agent").
		Complete(r)
}
