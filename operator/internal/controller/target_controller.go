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

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
	"github.com/epheo/stornas/operator/internal/rdrbd"
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
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.stornas.io,resources=targets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=r-drbd.io,resources=volumes,verbs=get;list;watch

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
				return ctrl.Result{}, nil
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
	versions, secretProblem, err := chapSecrets(ctx, r.Client, &target)
	if err != nil {
		return ctrl.Result{}, err
	}
	target.Status.ChapSecretVersions = versions

	unplaced := false
	vols := make([]*rdrbdv1alpha1.Volume, 0, len(target.Spec.LUNs))
	reason, msg := "", ""
	for _, lun := range target.Spec.LUNs {
		v, re, m := claimVolume(ctx, r.Client, target.Namespace, lun.ClaimName)
		if re != "" {
			reason, msg = re, m
			break
		}
		vols = append(vols, v)
	}

	switch {
	case reason != "":
		available.Reason = reason
		available.Message = msg
		if reason == storagev1alpha1.ReasonWaitingForVolume {
			res.RequeueAfter = volumeSettleInterval
		}

	default:
		avoid, err := unreadyNodes(ctx, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		luns := make([]storagev1alpha1.LUNStatus, 0, len(vols))
		node, replicated := "", false
		for i, v := range vols {
			n, device, replicas, err := rdrbd.ResolvePlacement(v, target.Status.ActiveNode, avoid)
			if err != nil {
				// No requeue: the Volume watch brings the next pass when
				// a replica can serve again.
				available.Reason = storagev1alpha1.ReasonWaitingForVolume
				available.Message = err.Error()
				unplaced = true
				break
			}
			if i == 0 {
				node = n
			}
			replicated = replicated || replicas > 1
			luns = append(luns, storagev1alpha1.LUNStatus{ID: target.Spec.LUNs[i].ID, Device: device})
		}
		switch {
		case unplaced:

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
			switch {
			case secretProblem != "":
				// Placement still stands: one broken Secret must not cut
				// off the other initiators.
				available.Reason = storagev1alpha1.ReasonInvalidSpec
				available.Message = secretProblem
			case meta.IsStatusConditionTrue(target.Status.Conditions, storagev1alpha1.ConditionHostReady):
				available.Status = metav1.ConditionTrue
				available.Reason = storagev1alpha1.ReasonReady
			default:
				available.Reason = storagev1alpha1.ReasonWaitingForAgent
				available.Message = "waiting for the node agent to export the target"
			}
		}
	}

	meta.SetStatusCondition(&target.Status.Conditions, available)
	if err := r.Status().Update(ctx, &target); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
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
		gone, err := nodeGoneOrUnready(ctx, r.Client, target.Status.ActiveNode)
		if err != nil {
			return ctrl.Result{}, err
		}
		done = gone
	}
	if !done {
		return ctrl.Result{RequeueAfter: volumeSettleInterval}, nil
	}
	controllerutil.RemoveFinalizer(target, teardownFinalizer)
	if err := r.Update(ctx, target); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
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

// chapSecrets returns the versions of the usable CHAP Secrets the
// initiators reference, and what is wrong with the first unusable one.
func chapSecrets(ctx context.Context, c client.Client, target *storagev1alpha1.Target) ([]storagev1alpha1.SecretVersion, string, error) {
	var versions []storagev1alpha1.SecretVersion
	problem := ""
	seen := map[string]bool{}
	for _, ini := range target.Spec.Initiators {
		ref := ini.ChapSecretRef
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		var secret corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: ref}, &secret)
		switch {
		case apierrors.IsNotFound(err):
			if problem == "" {
				problem = "chap secret " + ref + " not found"
			}
		case err != nil:
			return nil, "", err
		case len(secret.Data["username"]) == 0 || len(secret.Data["password"]) == 0:
			if problem == "" {
				problem = "chap secret " + ref + " has no username or password key"
			}
		default:
			versions = append(versions, storagev1alpha1.SecretVersion{Name: ref, ResourceVersion: secret.ResourceVersion})
		}
	}
	return versions, problem, nil
}

// secretTargets maps a Secret event to the targets whose initiators
// reference it.
func (r *TargetReconciler) secretTargets(ctx context.Context, obj client.Object) []reconcile.Request {
	var list storagev1alpha1.TargetList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, t := range list.Items {
		for _, ini := range t.Spec.Initiators {
			if ini.ChapSecretRef == obj.GetName() {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&t)})
				break
			}
		}
	}
	return reqs
}

// volumeTargets maps a Volume event to the targets exposing its claim.
func (r *TargetReconciler) volumeTargets(ctx context.Context, obj client.Object) []reconcile.Request {
	claim, ok := volumeClaim(ctx, r.Client, obj)
	if !ok {
		return nil
	}
	var list storagev1alpha1.TargetList
	if err := r.List(ctx, &list, client.InNamespace(claim.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, t := range list.Items {
		for _, lun := range t.Spec.LUNs {
			if lun.ClaimName == claim.Name {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&t)})
				break
			}
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *TargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Unfiltered on purpose: an update conflict returns without requeue
		// and relies on the competing write's event to reconcile again.
		For(&storagev1alpha1.Target{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.allTargets),
			builder.WithPredicates(nodeReadyChanged)).
		Watches(&rdrbdv1alpha1.Volume{}, handler.EnqueueRequestsFromMapFunc(r.volumeTargets),
			builder.WithPredicates(volumeServingChanged)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretTargets)).
		Named("target").
		Complete(r)
}
