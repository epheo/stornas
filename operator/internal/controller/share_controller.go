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
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// SharePlacer resolves where a LINSTOR resource should be served and which
// device to mount there; nil means no controller is configured.
type SharePlacer interface {
	ResolvePlacement(ctx context.Context, resource string) (node, device string, err error)
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

	handle, reason, msg := r.volumeHandle(ctx, &share)
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
		node, device, err := r.Linstor.ResolvePlacement(ctx, handle)
		if err != nil {
			available.Reason = storagev1alpha1.ReasonLinstorError
			available.Message = err.Error()
			retErr = err
			break
		}
		share.Status.Node = node
		share.Status.Device = device
		if meta.IsStatusConditionTrue(share.Status.Conditions, storagev1alpha1.ConditionHostReady) {
			available.Status = metav1.ConditionTrue
			available.Reason = storagev1alpha1.ReasonReady
		} else {
			available.Reason = storagev1alpha1.ReasonWaitingForAgent
			available.Message = "waiting for the node agent to mount and export"
		}
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

// volumeHandle resolves the share's PVC to its LINSTOR resource name (the
// CSI volume handle). A non-empty reason means the volume is not usable yet.
func (r *ShareReconciler) volumeHandle(ctx context.Context, share *storagev1alpha1.Share) (handle, reason, msg string) {
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Namespace: share.Namespace, Name: share.Spec.ClaimName}, &pvc)
	if err != nil {
		return "", storagev1alpha1.ReasonWaitingForVolume, "claim " + share.Spec.ClaimName + " not found"
	}
	if pvc.Spec.VolumeName == "" {
		return "", storagev1alpha1.ReasonWaitingForVolume, "claim " + share.Spec.ClaimName + " not bound"
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return "", storagev1alpha1.ReasonWaitingForVolume, "volume " + pvc.Spec.VolumeName + " not found"
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "linstor.csi.linbit.com" {
		return "", storagev1alpha1.ReasonInvalidSpec, "claim is not backed by LINSTOR CSI"
	}
	return pv.Spec.CSI.VolumeHandle, "", ""
}

// SetupWithManager sets up the controller with the Manager.
func (r *ShareReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Share{}).
		Named("share").
		Complete(r)
}
