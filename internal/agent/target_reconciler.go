package agent

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// TargetAgentReconciler exports targets placed on its node and tears them
// down when placement moves away, releasing the DRBD device and VIP so
// the new active node can take both.
type TargetAgentReconciler struct {
	client.Client
	Secrets client.Reader
	Node    string
	LIO     *LIOManager
}

func (r *TargetAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var target storagev1alpha1.Target
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.LIO.RemoveTarget(ctx, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if target.Status.ActiveNode != r.Node {
		r.LIO.TeardownTarget(ctx, target.Name, target.Spec.VIP)
		return ctrl.Result{}, nil
	}
	if len(target.Status.LUNs) == 0 {
		return ctrl.Result{}, nil
	}

	chap := map[string]ChapCred{}
	for _, ini := range target.Spec.Initiators {
		if ini.ChapSecretRef == "" {
			continue
		}
		var secret corev1.Secret
		if err := r.Secrets.Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: ini.ChapSecretRef}, &secret); err != nil {
			continue // the operator's condition surface owns missing-secret reporting
		}
		chap[ini.IQN] = ChapCred{
			User:     string(secret.Data["username"]),
			Password: string(secret.Data["password"]),
		}
	}

	ensureErr := r.LIO.EnsureTarget(ctx, &target, chap)

	cond := metav1.Condition{
		Type:               storagev1alpha1.ConditionHostReady,
		Status:             metav1.ConditionTrue,
		Reason:             storagev1alpha1.ReasonReady,
		ObservedGeneration: target.Generation,
	}
	target.Status.State = "Exported"
	if ensureErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = storagev1alpha1.ReasonHostError
		cond.Message = ensureErr.Error()
		target.Status.State = "Failed"
	}
	meta.SetStatusCondition(&target.Status.Conditions, cond)
	if err := r.Status().Update(ctx, &target); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if ensureErr != nil {
		return ctrl.Result{}, ensureErr
	}
	// Re-read CHAP secrets on a timer: rotation does not touch the Target.
	return ctrl.Result{RequeueAfter: secretRefreshInterval}, nil
}

func (r *TargetAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Target{}).
		Named("target-agent").
		Complete(r)
}
