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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// LinstorRegistrar registers a node's VG under the shared LINSTOR pool
// name. nil means no controller is configured; registration is skipped
// and reported as such rather than failed.
type LinstorRegistrar interface {
	EnsurePool(ctx context.Context, node, vg string) error
	DeletePool(ctx context.Context, node string) error
}

// poolFinalizer holds deletion until the LINSTOR catalog entry is gone;
// without it the registration needs manual cleanup after delete-recreate.
const poolFinalizer = "storage.stornas.io/linstor-deregister"

// StoragePoolReconciler owns every StoragePool condition except HostReady,
// which belongs to the node agent (internal/agent in the root module).
type StoragePoolReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Linstor LinstorRegistrar
}

// +kubebuilder:rbac:groups=storage.stornas.io,resources=storagepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.stornas.io,resources=storagepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.stornas.io,resources=storagepools/finalizers,verbs=update

func (r *StoragePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool storagev1alpha1.StoragePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !pool.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&pool, poolFinalizer) {
			if r.Linstor != nil {
				if err := r.Linstor.DeletePool(ctx, pool.Spec.Node); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(&pool, poolFinalizer)
			if err := r.Update(ctx, &pool); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&pool, poolFinalizer) {
		if err := r.Update(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
	}

	available := metav1.Condition{
		Type:               storagev1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: pool.Generation,
	}
	var retErr error

	switch {
	case len(pool.Spec.Devices) < storagev1alpha1.MinDevices(pool.Spec.Raid):
		available.Reason = storagev1alpha1.ReasonInvalidSpec
		available.Message = fmt.Sprintf("%s needs at least %d devices, got %d",
			pool.Spec.Raid, storagev1alpha1.MinDevices(pool.Spec.Raid), len(pool.Spec.Devices))

	case !meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionHostReady):
		// No requeue: the agent's status write triggers the next pass.
		available.Reason = storagev1alpha1.ReasonWaitingForAgent
		available.Message = "waiting for the node agent to converge host state"

	case r.Linstor == nil:
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionLinstorRegistered,
			Status:             metav1.ConditionUnknown,
			Reason:             storagev1alpha1.ReasonNotConfigured,
			Message:            "no LINSTOR controller configured",
			ObservedGeneration: pool.Generation,
		})
		available.Status = metav1.ConditionTrue
		available.Reason = storagev1alpha1.ReasonReady

	default:
		if err := r.Linstor.EnsurePool(ctx, pool.Spec.Node, pool.Status.VG); err != nil {
			meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ConditionLinstorRegistered,
				Status:             metav1.ConditionFalse,
				Reason:             storagev1alpha1.ReasonLinstorError,
				Message:            err.Error(),
				ObservedGeneration: pool.Generation,
			})
			available.Reason = storagev1alpha1.ReasonLinstorError
			available.Message = err.Error()
			retErr = err
		} else {
			pool.Status.LinstorPool = storagev1alpha1.LinstorPool
			meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ConditionLinstorRegistered,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ReasonReady,
				ObservedGeneration: pool.Generation,
			})
			available.Status = metav1.ConditionTrue
			available.Reason = storagev1alpha1.ReasonReady
		}
	}

	meta.SetStatusCondition(&pool.Status.Conditions, available)
	if err := r.Status().Update(ctx, &pool); err != nil {
		// The agent writes this status on a timer, so conflicts are routine.
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, retErr
}

// SetupWithManager sets up the controller with the Manager.
func (r *StoragePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.StoragePool{}).
		Named("storagepool").
		Complete(r)
}
