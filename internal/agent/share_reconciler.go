package agent

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// ShareAgentReconciler executes the operator's placement on its own node:
// mount, NFS export, samba include. It writes HostReady and State; every
// other Share condition belongs to the operator.
type ShareAgentReconciler struct {
	client.Client
	Node   string
	Shares *ShareManager
}

func (r *ShareAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var share storagev1alpha1.Share
	if err := r.Get(ctx, req.NamespacedName, &share); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Deleted: tear down local state and regenerate the samba
			// include from what remains.
			r.Shares.RemoveShare(ctx, req.Namespace, req.Name)
			_ = r.applySamba(ctx)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if share.Status.Node != r.Node || share.Status.Device == "" {
		return ctrl.Result{}, nil
	}

	ensureErr := r.Shares.EnsureShare(ctx, &share)
	if ensureErr == nil {
		ensureErr = r.applySamba(ctx)
	}

	cond := metav1.Condition{
		Type:               storagev1alpha1.ConditionHostReady,
		Status:             metav1.ConditionTrue,
		Reason:             storagev1alpha1.ReasonReady,
		ObservedGeneration: share.Generation,
	}
	share.Status.State = "Exported"
	if ensureErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = storagev1alpha1.ReasonHostError
		cond.Message = ensureErr.Error()
		share.Status.State = "Failed"
	}
	meta.SetStatusCondition(&share.Status.Conditions, cond)
	if err := r.Status().Update(ctx, &share); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, ensureErr
}

func (r *ShareAgentReconciler) applySamba(ctx context.Context) error {
	var list storagev1alpha1.ShareList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
	return r.Shares.ApplySamba(ctx, list.Items)
}

func (r *ShareAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Share{}).
		Named("share-agent").
		Complete(r)
}
