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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
	"github.com/epheo/stornas/operator/internal/rdrbd"
)

// poolFinalizer holds deletion until the r-drbd Pool is gone; the agent
// may only wipe the VG after that.
const poolFinalizer = "storage.stornas.io/deregister"

// legacyPoolFinalizer is released on delete only to unblock pools left by
// a LINSTOR build; no data migration is implied.
const legacyPoolFinalizer = "storage.stornas.io/linstor-deregister"

// StoragePoolReconciler owns every StoragePool condition except HostReady,
// which belongs to the node agent (internal/agent in the root module).
type StoragePoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.stornas.io,resources=storagepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.stornas.io,resources=storagepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.stornas.io,resources=storagepools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=r-drbd.io,resources=pools,verbs=get;list;watch;create;delete

func (r *StoragePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool storagev1alpha1.StoragePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !pool.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&pool, poolFinalizer) {
			if controllerutil.RemoveFinalizer(&pool, legacyPoolFinalizer) {
				return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, &pool))
			}
			return ctrl.Result{}, nil
		}
		gone, msg, err := rdrbd.DeletePool(ctx, r.Client, &pool)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !gone {
			// The Pool watch brings the next pass; the condition says why
			// deletion hangs.
			meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ConditionDeregistered,
				Status:             metav1.ConditionFalse,
				Reason:             storagev1alpha1.ReasonWaitingForPool,
				Message:            msg,
				ObservedGeneration: pool.Generation,
			})
			if err := r.Status().Update(ctx, &pool); err != nil && !apierrors.IsConflict(err) {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			return ctrl.Result{}, nil
		}
		// Deregistered is the agent's go-ahead to wipe: until it is set,
		// r-drbd may still back live volumes with this VG, and the agent
		// wiping concurrently would destroy them.
		if !meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionDeregistered) {
			meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ConditionDeregistered,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ReasonReady,
				ObservedGeneration: pool.Generation,
			})
			if err := r.Status().Update(ctx, &pool); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			return ctrl.Result{RequeueAfter: volumeSettleInterval}, nil
		}
		// The agent wipes the host next (the spec is the map of what to
		// wipe) and confirms with TornDown. A dead node cannot confirm,
		// and its host state is unreachable anyway.
		if !meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionTornDown) {
			gone, err := nodeGoneOrUnready(ctx, r.Client, pool.Spec.Node)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !gone {
				return ctrl.Result{RequeueAfter: volumeSettleInterval}, nil
			}
		}
		controllerutil.RemoveFinalizer(&pool, poolFinalizer)
		controllerutil.RemoveFinalizer(&pool, legacyPoolFinalizer)
		if err := r.Update(ctx, &pool); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&pool, poolFinalizer) {
		if err := r.Update(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The failure matrix promises an alert when a pool degrades; the UI's
	// alert feed is Warning events, so unhealthy health (written by the
	// agent) emits one, naming the bad members. The recorder's event
	// series absorbs the repeats.
	if r.Recorder != nil && (pool.Status.Health == "Degraded" || pool.Status.Health == "Failed") {
		msg := "pool " + pool.Name + " is " + pool.Status.Health
		for _, d := range pool.Status.Devices {
			if d.State == "Missing" || d.State == "Failed" {
				msg += "; " + d.Path + " " + d.State
			}
		}
		r.Recorder.Eventf(&pool, nil, corev1.EventTypeWarning, "Pool"+pool.Status.Health, "Degrade", "%s", msg)
	}

	available := metav1.Condition{
		Type:               storagev1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: pool.Generation,
	}
	var backendErr error
	switch {
	case len(pool.Spec.Devices) < storagev1alpha1.MinDevices(pool.Spec.Raid):
		available.Reason = storagev1alpha1.ReasonInvalidSpec
		available.Message = fmt.Sprintf("%s needs at least %d devices, got %d",
			pool.Spec.Raid, storagev1alpha1.MinDevices(pool.Spec.Raid), len(pool.Spec.Devices))

	case pool.Spec.Thin != nil && !*pool.Spec.Thin:
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionRegistered,
			Status:             metav1.ConditionFalse,
			Reason:             storagev1alpha1.ReasonUnsupported,
			Message:            "r-drbd pools are thin only",
			ObservedGeneration: pool.Generation,
		})
		available.Reason = storagev1alpha1.ReasonUnsupported
		available.Message = "r-drbd pools are thin only"

	case !meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionHostReady):
		// The VG may be gone; a stale Registered=True would claim otherwise.
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionRegistered,
			Status:             metav1.ConditionUnknown,
			Reason:             storagev1alpha1.ReasonHostNotReady,
			Message:            "waiting for the node agent to converge host state",
			ObservedGeneration: pool.Generation,
		})
		// No requeue: the agent's status write triggers the next pass.
		available.Reason = storagev1alpha1.ReasonWaitingForAgent
		available.Message = "waiting for the node agent to converge host state"

	default:
		registered := metav1.Condition{
			Type:               storagev1alpha1.ConditionRegistered,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
		}
		// No requeue on a false ready: the Pool watch brings the next pass
		// when the r-drbd agent reports.
		ready, msg, err := rdrbd.EnsurePool(ctx, r.Client, &pool)
		backendErr = err
		switch {
		case err != nil:
			registered.Reason = storagev1alpha1.ReasonBackendError
			registered.Message = err.Error()
		case !ready:
			registered.Reason = storagev1alpha1.ReasonWaitingForPool
			registered.Message = msg
		default:
			registered.Status = metav1.ConditionTrue
			registered.Reason = storagev1alpha1.ReasonReady
			available.Status = metav1.ConditionTrue
			available.Reason = storagev1alpha1.ReasonReady
		}
		if registered.Status != metav1.ConditionTrue {
			available.Reason = registered.Reason
			available.Message = registered.Message
		}
		meta.SetStatusCondition(&pool.Status.Conditions, registered)
	}

	meta.SetStatusCondition(&pool.Status.Conditions, available)
	if err := r.Status().Update(ctx, &pool); err != nil {
		// The agent writes this status on a timer, so conflicts are routine.
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, backendErr
}

// SetupWithManager sets up the controller with the Manager.
func (r *StoragePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Unfiltered on purpose: an update conflict returns without requeue
		// and relies on the competing write's event to reconcile again.
		For(&storagev1alpha1.StoragePool{}).
		// A Pool keeps its StoragePool's name, so its events map straight
		// back: Available flips and the release on delete.
		Watches(&rdrbdv1alpha1.Pool{}, &handler.EnqueueRequestForObject{}).
		Named("storagepool").
		Complete(r)
}
