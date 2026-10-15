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
	"sort"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// selectedNodeAnnotation is what the scheduler writes for WFFC claims and
// what the CSI external-provisioner waits on before calling CreateVolume.
// De facto API, not a documented contract.
const selectedNodeAnnotation = "volume.kubernetes.io/selected-node"

// ClaimBinder completes WaitForFirstConsumer for claims the host consumes
// (nfsd, LIO): no pod ever schedules them, so without selected-node the
// provisioner never calls r-drbd. Only claims declaring
// ConsumerAnnotation=host are touched; the node is where stornas would
// serve them, and r-drbd's own pins (pool, snapshot) still win placement.
type ClaimBinder struct {
	client.Client
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

func (r *ClaimBinder) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !hostClaimPending(&pvc) {
		return ctrl.Result{}, nil
	}
	sc, err := r.wffcClass(ctx, &pvc)
	if err != nil || sc == nil {
		return ctrl.Result{}, err
	}
	node, err := r.pickNode(ctx, &pvc, sc)
	if err != nil || node == "" {
		// No eligible node yet: a StoragePool or Node event re-enqueues.
		return ctrl.Result{}, err
	}
	// Patch the one key: the PVC is shared with the scheduler and the
	// provisioner, and a whole-object update would claim their fields.
	// The resourceVersion guard keeps a stale cache from placing twice.
	patch := client.MergeFromWithOptions(pvc.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[selectedNodeAnnotation] = node
	if err := r.Patch(ctx, &pvc, patch); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("bound host-consumed claim", "claim", pvc.Name, "node", node)
	return ctrl.Result{}, nil
}

// hostClaimPending: declared host consumer, not bound, not yet placed.
func hostClaimPending(pvc *corev1.PersistentVolumeClaim) bool {
	return pvc.DeletionTimestamp.IsZero() && pvc.Spec.VolumeName == "" &&
		pvc.Annotations[selectedNodeAnnotation] == "" &&
		pvc.Annotations[storagev1alpha1.ConsumerAnnotation] == storagev1alpha1.ConsumerHost
}

// wffcClass returns the claim's class when it is an r-drbd WFFC class;
// Immediate classes bind themselves and other drivers are not ours.
func (r *ClaimBinder) wffcClass(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*storagev1.StorageClass, error) {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return nil, nil
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, &sc); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if sc.Provisioner != rdrbdv1alpha1.Driver || sc.VolumeBindingMode == nil ||
		*sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		return nil, nil
	}
	return &sc, nil
}

// pickNode returns "" when no node qualifies yet.
func (r *ClaimBinder) pickNode(ctx context.Context, pvc *corev1.PersistentVolumeClaim, sc *storagev1.StorageClass) (string, error) {
	var pools storagev1alpha1.StoragePoolList
	if err := r.List(ctx, &pools); err != nil {
		return "", err
	}
	pinned := pvc.Annotations[storagev1alpha1.PoolAnnotation]
	if pinned != "" {
		for _, p := range pools.Items {
			if p.Name == pinned && meta.IsStatusConditionTrue(p.Status.Conditions, storagev1alpha1.ConditionAvailable) {
				return p.Spec.Node, nil
			}
		}
		return "", nil
	}
	tier := sc.Parameters[rdrbdv1alpha1.ParamTier]
	if tier == "" {
		tier = storagev1alpha1.DefaultTier
	}
	// Best pool free space per Ready node holding the tier. A replicated
	// volume or a restore is placed by r-drbd regardless of this node
	// (any diskful node serves; restores follow their snapshot), so any
	// such node will do and the ranking only makes the choice stable.
	free := map[string]int64{}
	for _, p := range pools.Items {
		ptier := p.Spec.Tier
		if ptier == "" {
			ptier = storagev1alpha1.DefaultTier
		}
		if ptier != tier ||
			!meta.IsStatusConditionTrue(p.Status.Conditions, storagev1alpha1.ConditionAvailable) {
			continue
		}
		f := int64(0)
		if p.Status.Free != nil {
			f = p.Status.Free.Value()
		}
		if cur, ok := free[p.Spec.Node]; !ok || f > cur {
			free[p.Spec.Node] = f
		}
	}
	nodes := make([]string, 0, len(free))
	for n := range free {
		gone, err := nodeGoneOrUnready(ctx, r.Client, n)
		if err != nil {
			return "", err
		}
		if !gone {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return "", nil
	}
	sort.Slice(nodes, func(i, j int) bool {
		if free[nodes[i]] != free[nodes[j]] {
			return free[nodes[i]] > free[nodes[j]]
		}
		return nodes[i] < nodes[j]
	})
	return nodes[0], nil
}

// pendingHostClaims maps a pool, class or node event to every claim still
// waiting, so a pool going Available or a node returning binds them.
func (r *ClaimBinder) pendingHostClaims(ctx context.Context, _ client.Object) []reconcile.Request {
	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range pvcs.Items {
		if hostClaimPending(&pvcs.Items[i]) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pvcs.Items[i])})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaimBinder) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}).
		Watches(&storagev1alpha1.StoragePool{}, handler.EnqueueRequestsFromMapFunc(r.pendingHostClaims)).
		// A host claim created before its class binds once the class exists.
		Watches(&storagev1.StorageClass{}, handler.EnqueueRequestsFromMapFunc(r.pendingHostClaims)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.pendingHostClaims),
			builder.WithPredicates(nodeReadyChanged)).
		Named("claimbinder").
		Complete(r)
}
