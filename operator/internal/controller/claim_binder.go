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
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// selectedNodeAnnotation is what the scheduler writes for WFFC claims and
// what the CSI external-provisioner waits on. De facto API, not a
// documented contract; if sig-storage ever breaks it the fallback is
// Immediate-binding classes with a migration.
const selectedNodeAnnotation = "volume.kubernetes.io/selected-node"

const linstorProvisioner = "linstor.csi.linbit.com"

// defaultBindGrace lets a consumer pod created moments after its claim
// reach the API before the binder decides, so pod-first placement wins
// whenever a pod exists.
const defaultBindGrace = 10 * time.Second

// ClaimBinder completes WaitForFirstConsumer for claims that will never
// see a pod: UI volumes, CDI imports, any bare PVC. A claim a pod
// references is left to the scheduler; everything else gets a node picked
// here, because placement is a decision and decisions live in the
// operator.
type ClaimBinder struct {
	client.Client
	Linstor SharePlacer
	// Grace overrides defaultBindGrace; tests shrink it.
	Grace time.Duration
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch

func (r *ClaimBinder) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// VolumeName covers Bound and Lost; envtest never sets Phase, so the
	// spec is the signal, not the phase.
	if !pvc.DeletionTimestamp.IsZero() || pvc.Spec.VolumeName != "" ||
		pvc.Annotations[selectedNodeAnnotation] != "" {
		return ctrl.Result{}, nil
	}
	eligible, err := r.eligibleClass(ctx, &pvc)
	if err != nil || !eligible {
		return ctrl.Result{}, err
	}
	grace := r.Grace
	if grace == 0 {
		grace = defaultBindGrace
	}
	if wait := grace - time.Since(pvc.CreationTimestamp.Time); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	referenced, err := r.claimHasConsumer(ctx, &pvc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if referenced {
		// The scheduler owns this claim. Keep polling: if the pod is
		// deleted before scheduling, the claim is ours again.
		return ctrl.Result{RequeueAfter: volumeSettleInterval}, nil
	}
	node, err := r.pickNode(ctx, &pvc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if node == "" {
		// No eligible pool, or a restore whose source cannot be resolved
		// yet. Fail closed and keep trying; binding somewhere wrong would
		// strand the data or the restore.
		return ctrl.Result{RequeueAfter: volumeSettleInterval}, nil
	}
	// The PVC is the only object stornas writes without owning it: the CSI
	// provisioner, the scheduler, and CDI write it too. Patch the one key
	// so managedFields records this operator against the hint alone, not
	// against every field a whole-object update round-trips.
	patch := client.MergeFrom(pvc.DeepCopy())
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[selectedNodeAnnotation] = node
	if err := r.Patch(ctx, &pvc, patch); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("bound podless claim", "claim", pvc.Name, "node", node)
	return ctrl.Result{}, nil
}

// eligibleClass limits the binder to our own WFFC classes; Immediate
// classes bind themselves and foreign provisioners are not ours to place.
func (r *ClaimBinder) eligibleClass(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, error) {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return false, nil
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, &sc); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return sc.Provisioner == linstorProvisioner &&
		sc.VolumeBindingMode != nil &&
		*sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer, nil
}

func (r *ClaimBinder) claimHasConsumer(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(pvc.Namespace)); err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvc.Name {
				return true, nil
			}
		}
	}
	return false, nil
}

// pickNode returns "" when no safe choice exists; the caller polls.
func (r *ClaimBinder) pickNode(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (string, error) {
	avoid, err := unreadyNodes(ctx, r.Client)
	if err != nil {
		return "", err
	}
	// A restore must land where its snapshot lives: LINSTOR snapshots are
	// node-local for the local class, and a wrong node strands the restore
	// in provisioning forever.
	if src := pvc.Spec.DataSource; src != nil && src.Kind == "VolumeSnapshot" && r.Linstor != nil {
		node, err := r.snapshotNode(ctx, pvc.Namespace, src.Name, avoid)
		if err != nil {
			logf.FromContext(ctx).Info("restore source unresolved, holding bind",
				"claim", pvc.Name, "snapshot", src.Name, "err", err.Error())
			return "", nil
		}
		return node, nil
	}
	var pools storagev1alpha1.StoragePoolList
	if err := r.List(ctx, &pools); err != nil {
		return "", err
	}
	eligible := pools.Items[:0]
	for _, p := range pools.Items {
		if !meta.IsStatusConditionTrue(p.Status.Conditions, storagev1alpha1.ConditionAvailable) {
			continue
		}
		if avoid[p.Spec.Node] || p.Status.Health == "Failed" || p.Status.Free == nil {
			continue
		}
		eligible = append(eligible, p)
	}
	if len(eligible) == 0 {
		return "", nil
	}
	sort.Slice(eligible, func(i, j int) bool {
		fi, fj := eligible[i].Status.Free.Value(), eligible[j].Status.Free.Value()
		if fi != fj {
			return fi > fj
		}
		return eligible[i].Name < eligible[j].Name
	})
	return eligible[0].Spec.Node, nil
}

// snapshotNode walks snapshot -> source claim -> LINSTOR resource and asks
// placement for a diskful node; any missing link fails the bind closed.
func (r *ClaimBinder) snapshotNode(ctx context.Context, namespace, snapshot string, avoid map[string]bool) (string, error) {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot",
	})
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: snapshot}, snap); err != nil {
		return "", err
	}
	src, _, _ := unstructured.NestedString(snap.Object, "spec", "source", "persistentVolumeClaimName")
	if src == "" {
		return "", errNoSnapshotSource
	}
	handle, reason, msg := claimHandle(ctx, r.Client, namespace, src)
	if reason != "" {
		return "", &bindHoldError{msg}
	}
	node, _, _, err := r.Linstor.ResolvePlacement(ctx, handle, "", avoid)
	if err != nil {
		return "", err
	}
	return node, nil
}

var errNoSnapshotSource = &bindHoldError{"snapshot has no source claim"}

// bindHoldError only feeds log lines; holding is signalled by "".
type bindHoldError struct{ msg string }

func (e *bindHoldError) Error() string { return e.msg }

// SetupWithManager sets up the controller with the Manager.
func (r *ClaimBinder) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}).
		Named("claimbinder").
		Complete(r)
}
