/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// teardownFinalizer holds Target deletion open until the active agent
// sheds the export and VIP: the spec is the only place the VIP lives, so
// it must outlive the teardown or the address lingers on the node.
const teardownFinalizer = "storage.stornas.io/teardown"

// TargetReconciler owns placement and device resolution for iSCSI targets.
// v1 failover is active/passive: every LUN is served from one node (the
// first LUN's DRBD primary), the agent raises the VIP there, initiators
// reconnect. When the active node goes NotReady, placement re-resolves
// away from it; the agent on the loser tears down, the winner exports and
// takes the VIP. DRBD device paths are node-agnostic, so colocating LUNs
// whose primaries differ still works; promotion happens on open.
type TargetReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Linstor SharePlacer
}

// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *TargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var target storagev1alpha1.Target
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !target.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &target)
	}
	if controllerutil.AddFinalizer(&target, teardownFinalizer) {
		if err := r.Update(ctx, &target); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
	}

	available := metav1.Condition{
		Type:               storagev1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: target.Generation,
	}
	res := ctrl.Result{}

	target.Status.IQN = storagev1alpha1.IQNPrefix + target.Name

	linstorFailed := false
	handles := make([]string, 0, len(target.Spec.LUNs))
	reason, msg := "", ""
	for _, lun := range target.Spec.LUNs {
		h, re, m := claimHandle(ctx, r.Client, target.Namespace, lun.ClaimName)
		if re != "" {
			reason, msg = re, m
			break
		}
		handles = append(handles, h)
	}

	switch {
	case reason != "":
		available.Reason = reason
		available.Message = msg
		if reason == storagev1alpha1.ReasonWaitingForVolume {
			res.RequeueAfter = volumeSettleInterval
		}

	case r.Linstor == nil:
		available.Reason = storagev1alpha1.ReasonNotConfigured
		available.Message = "no LINSTOR controller configured"

	default:
		avoid, err := unreadyNodes(ctx, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		luns := make([]storagev1alpha1.LUNStatus, 0, len(handles))
		node, replicated := "", false
		for i, h := range handles {
			n, device, replicas, err := r.Linstor.ResolvePlacement(ctx, h, target.Status.ActiveNode, avoid)
			if err != nil {
				// Flat requeue, not an error: LINSTOR heals on its own
				// schedule, and exponential backoff would strand
				// placement for minutes after it recovers.
				available.Reason = storagev1alpha1.ReasonLinstorError
				available.Message = err.Error()
				res.RequeueAfter = volumeSettleInterval
				linstorFailed = true
				break
			}
			if i == 0 {
				node = n
			}
			replicated = replicated || replicas > 1
			luns = append(luns, storagev1alpha1.LUNStatus{ID: target.Spec.LUNs[i].ID, Device: device})
		}
		switch {
		case linstorFailed:

		case replicated && target.Spec.VIP == "":
			// Without a VIP initiators cannot follow a failover, so a
			// replicated LUN behind a fixed node address is a spec error.
			available.Reason = storagev1alpha1.ReasonInvalidSpec
			available.Message = "vip is required when a LUN is replicated"

		default:
			moved := target.Status.ActiveNode != "" && target.Status.ActiveNode != node
			target.Status.ActiveNode = node
			target.Status.LUNs = luns
			if moved {
				// The old node's HostReady is stale the moment placement
				// moves; reset it so Available tracks the new node's agent.
				meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
					Type:               storagev1alpha1.ConditionHostReady,
					Status:             metav1.ConditionFalse,
					Reason:             storagev1alpha1.ReasonWaitingForAgent,
					Message:            "moved to " + node,
					ObservedGeneration: target.Generation,
				})
			}
			if meta.IsStatusConditionTrue(target.Status.Conditions, storagev1alpha1.ConditionHostReady) {
				available.Status = metav1.ConditionTrue
				available.Reason = storagev1alpha1.ReasonReady
			} else {
				available.Reason = storagev1alpha1.ReasonWaitingForAgent
				available.Message = "waiting for the node agent to export the target"
			}
			res.RequeueAfter = placementRecheckInterval
		}
	}

	meta.SetStatusCondition(&target.Status.Conditions, available)
	if err := r.Status().Update(ctx, &target); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return res, nil
}

// reconcileDelete releases the finalizer once the active agent confirms
// teardown (State Removed), placement never happened, or the active node
// is unready: a dead node cannot confirm, its exports died with it, and a
// reboot clears the VIP. Known limit: a node that returns from a partition
// without rebooting sheds the export (agent sees NotFound) but not the
// VIP, which only the spec knew.
func (r *TargetReconciler) reconcileDelete(ctx context.Context, target *storagev1alpha1.Target) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(target, teardownFinalizer) {
		return ctrl.Result{}, nil
	}
	done := target.Status.ActiveNode == "" || target.Status.State == "Removed"
	if !done {
		unready, err := unreadyNodes(ctx, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		done = unready[target.Status.ActiveNode]
	}
	if !done {
		return ctrl.Result{RequeueAfter: volumeSettleInterval}, nil
	}
	controllerutil.RemoveFinalizer(target, teardownFinalizer)
	if err := r.Update(ctx, target); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// allTargets re-reconciles every target on a node readiness flip; the
// fleet is a handful of exports, so a full sweep beats tracking who served
// what.
func (r *TargetReconciler) allTargets(ctx context.Context, _ client.Object) []reconcile.Request {
	var list storagev1alpha1.TargetList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, t := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: t.Namespace, Name: t.Name},
		})
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *TargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Target{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.allTargets),
			builder.WithPredicates(nodeReadyChanged)).
		Named("target").
		Complete(r)
}
