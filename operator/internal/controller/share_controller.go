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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// SharePlacer resolves where a LINSTOR resource should be served and which
// device to mount there; nil means no controller is configured. prefer
// keeps placement sticky, avoid excludes unready nodes, replicas reports
// the diskful copy count.
type SharePlacer interface {
	ResolvePlacement(ctx context.Context, resource, prefer string, avoid map[string]bool) (node, device string, replicas int, err error)
}

// ShareReconciler owns placement and the Available condition; the agent on
// the placed node mounts and exports, reporting HostReady and State.
type ShareReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Linstor SharePlacer
}

// volumeSettleInterval paces re-checks while the PVC binds; there is no
// PVC watch wired, so waiting states poll.
const volumeSettleInterval = 15 * time.Second

// +kubebuilder:rbac:groups=storage.stornas.io,resources=shares,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.stornas.io,resources=shares/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.stornas.io,resources=shares/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (r *ShareReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var share storagev1alpha1.Share
	if err := r.Get(ctx, req.NamespacedName, &share); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	available := metav1.Condition{
		Type:               storagev1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: share.Generation,
	}
	res := ctrl.Result{}
	var retErr error

	handle, reason, msg := claimHandle(ctx, r.Client, share.Namespace, share.Spec.ClaimName)
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
		node, device, _, err := r.Linstor.ResolvePlacement(ctx, handle, share.Status.Node, avoid)
		if err != nil {
			available.Reason = storagev1alpha1.ReasonLinstorError
			available.Message = err.Error()
			retErr = err
			break
		}
		moved := share.Status.Node != "" && share.Status.Node != node
		share.Status.Node = node
		share.Status.Device = device
		if moved {
			// The old node's HostReady is stale the moment placement
			// moves; reset it so Available tracks the new node's agent.
			meta.SetStatusCondition(&share.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ConditionHostReady,
				Status:             metav1.ConditionFalse,
				Reason:             storagev1alpha1.ReasonWaitingForAgent,
				Message:            "moved to " + node,
				ObservedGeneration: share.Generation,
			})
		}
		if meta.IsStatusConditionTrue(share.Status.Conditions, storagev1alpha1.ConditionHostReady) {
			available.Status = metav1.ConditionTrue
			available.Reason = storagev1alpha1.ReasonReady
		} else {
			available.Reason = storagev1alpha1.ReasonWaitingForAgent
			available.Message = "waiting for the node agent to mount and export"
		}
		res.RequeueAfter = placementRecheckInterval
	}

	meta.SetStatusCondition(&share.Status.Conditions, available)
	if err := r.Status().Update(ctx, &share); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return res, retErr
}

// allShares re-reconciles every share on a node readiness flip; the fleet
// is a handful of exports, so a full sweep beats tracking who served what.
func (r *ShareReconciler) allShares(ctx context.Context, _ client.Object) []reconcile.Request {
	var list storagev1alpha1.ShareList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, s := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name},
		})
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ShareReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Share{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.allShares),
			builder.WithPredicates(nodeReadyChanged)).
		Named("share").
		Complete(r)
}
