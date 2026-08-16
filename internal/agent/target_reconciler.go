package agent

import (
	"context"
	"fmt"

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

	// applied remembers the converged input per target so quiet
	// secretRefreshInterval ticks skip the targetcli walk (a dozen
	// ~1s python spawns plus a saveconfig rewrite, per target, forever).
	// In-memory on purpose: an agent restart reconverges everything,
	// which is also what repairs hand-edited host state after a reboot.
	// Single controller goroutine, so no lock.
	applied map[types.NamespacedName]string
}

func (r *TargetAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var target storagev1alpha1.Target
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		if client.IgnoreNotFound(err) == nil {
			delete(r.applied, req.NamespacedName)
			r.LIO.RemoveTarget(ctx, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if target.DeletionTimestamp != nil {
		delete(r.applied, req.NamespacedName)
		// The operator's finalizer keeps the spec (the only place the
		// VIP lives) alive until this teardown; Removed is the confirm
		// signal that releases it. Standby probes stay quiet.
		r.LIO.TeardownTarget(ctx, target.Name, target.Spec.VIP)
		if target.Status.ActiveNode == r.Node && target.Status.State != "Removed" {
			target.Status.State = "Removed"
			if err := r.Status().Update(ctx, &target); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}
	if target.Status.ActiveNode != r.Node {
		// Losing placement invalidates the cache: coming back must be a
		// full convergence, not a skip.
		delete(r.applied, req.NamespacedName)
		r.LIO.TeardownTarget(ctx, target.Name, target.Spec.VIP)
		return ctrl.Result{}, nil
	}
	if len(target.Status.LUNs) == 0 {
		return ctrl.Result{}, nil
	}

	chap := map[string]ChapCred{}
	// key captures every EnsureTarget input: spec (generation), placement
	// (LUN devices resolve from status), and secret rotation (RVs).
	key := fmt.Sprintf("g%d", target.Generation)
	for _, lun := range target.Status.LUNs {
		key += fmt.Sprintf("|%d:%s", lun.ID, lun.Device)
	}
	for _, ini := range target.Spec.Initiators {
		if ini.ChapSecretRef == "" {
			continue
		}
		var secret corev1.Secret
		if err := r.Secrets.Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: ini.ChapSecretRef}, &secret); err != nil {
			key += "|" + ini.ChapSecretRef + ":absent"
			continue // the operator's condition surface owns missing-secret reporting
		}
		key += "|" + ini.ChapSecretRef + ":" + secret.ResourceVersion
		chap[ini.IQN] = ChapCred{
			User:     string(secret.Data["username"]),
			Password: string(secret.Data["password"]),
		}
	}
	if r.applied[req.NamespacedName] == key {
		// Converged and nothing moved: the tick only had to re-read the
		// secrets above.
		return ctrl.Result{RequeueAfter: secretRefreshInterval}, nil
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
	if r.applied == nil {
		r.applied = map[types.NamespacedName]string{}
	}
	r.applied[req.NamespacedName] = key
	// Re-read CHAP secrets on a timer: rotation does not touch the Target.
	return ctrl.Result{RequeueAfter: secretRefreshInterval}, nil
}

func (r *TargetAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Target{}).
		Named("target-agent").
		Complete(r)
}
