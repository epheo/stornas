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
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// TargetReconciler owns placement and device resolution for iSCSI targets.
// v1 failover is active/passive: every LUN is served from one node (the
// first LUN's DRBD primary), the agent raises the VIP there, initiators
// reconnect. DRBD device paths are node-agnostic, so colocating LUNs whose
// primaries differ still works; the mount-side promotion happens on open.
type TargetReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Linstor SharePlacer
}

// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets/finalizers,verbs=update

const iqnPrefix = "iqn.2026-08.io.stornas:"

func (r *TargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var target storagev1alpha1.Target
	if err := r.Get(ctx, req.NamespacedName, &target); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	available := metav1.Condition{
		Type:               storagev1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: target.Generation,
	}
	res := ctrl.Result{}
	var retErr error

	target.Status.IQN = iqnPrefix + target.Name

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
		luns := make([]storagev1alpha1.LUNStatus, 0, len(handles))
		node := ""
		for i, h := range handles {
			n, device, err := r.Linstor.ResolvePlacement(ctx, h)
			if err != nil {
				available.Reason = storagev1alpha1.ReasonLinstorError
				available.Message = err.Error()
				retErr = err
				break
			}
			if i == 0 {
				node = n
			}
			luns = append(luns, storagev1alpha1.LUNStatus{ID: target.Spec.LUNs[i].ID, Device: device})
		}
		if retErr == nil {
			target.Status.ActiveNode = node
			target.Status.LUNs = luns
			if meta.IsStatusConditionTrue(target.Status.Conditions, storagev1alpha1.ConditionHostReady) {
				available.Status = metav1.ConditionTrue
				available.Reason = storagev1alpha1.ReasonReady
			} else {
				available.Reason = storagev1alpha1.ReasonWaitingForAgent
				available.Message = "waiting for the node agent to export the target"
			}
		}
	}

	meta.SetStatusCondition(&target.Status.Conditions, available)
	if err := r.Status().Update(ctx, &target); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return res, retErr
}

// claimHandle resolves a PVC to its LINSTOR resource name; shared by the
// Share and Target reconcilers. A non-empty reason means not usable yet.
func claimHandle(ctx context.Context, c client.Reader, namespace, claim string) (handle, reason, msg string) {
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claim}, &pvc); err != nil {
		return "", storagev1alpha1.ReasonWaitingForVolume, "claim " + claim + " not found"
	}
	if pvc.Spec.VolumeName == "" {
		return "", storagev1alpha1.ReasonWaitingForVolume, "claim " + claim + " not bound"
	}
	var pv corev1.PersistentVolume
	if err := c.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return "", storagev1alpha1.ReasonWaitingForVolume, "volume " + pvc.Spec.VolumeName + " not found"
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "linstor.csi.linbit.com" {
		return "", storagev1alpha1.ReasonInvalidSpec, "claim is not backed by LINSTOR CSI"
	}
	return pv.Spec.CSI.VolumeHandle, "", ""
}

// SetupWithManager sets up the controller with the Manager.
func (r *TargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.Target{}).
		Named("target").
		Complete(r)
}
