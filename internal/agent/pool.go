// Package agent converges host state to CRD specs. It is the dumb half of
// the split: all decisions live in the operator, the agent only executes
// and observes (CLAUDE.md invariants).
package agent

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/lvm"
)

// Report is what one convergence pass observed on the host.
type Report struct {
	VG       string
	Capacity int64
	Free     int64
	Health   string
	Devices  []storagev1alpha1.DeviceStatus
}

// EnsurePool converges LVM state for one pool and reports what it saw.
// It never removes or shrinks anything: pool deletion stays a human
// decision, so the agent only creates and observes.
func EnsurePool(ctx context.Context, l *lvm.LVM, pool *storagev1alpha1.StoragePool) (Report, error) {
	vg := pool.VGName()
	rep := Report{VG: vg, Health: "Failed"}

	for _, dev := range pool.Spec.Devices {
		if !l.IsPV(ctx, dev) {
			if err := l.CreatePV(ctx, dev); err != nil {
				return rep, err
			}
		}
	}
	if !l.VGExists(ctx, vg) {
		if err := l.CreateVG(ctx, vg, pool.Spec.Devices); err != nil {
			return rep, err
		}
	}
	if !l.LVExists(ctx, vg, storagev1alpha1.ThinLV) {
		if err := l.CreateThinPool(ctx, vg, storagev1alpha1.ThinLV, pool.Spec.Raid); err != nil {
			return rep, err
		}
	}

	info, err := l.VGInfo(ctx, vg)
	if err != nil {
		return rep, err
	}
	pvs, err := l.PVs(ctx, vg)
	if err != nil {
		return rep, err
	}

	rep.Capacity = info.SizeBytes
	rep.Free = info.FreeBytes
	rep.Health = "Online"
	for _, pv := range pvs {
		state := "InSync"
		if pv.Missing {
			state = "Missing"
			rep.Health = "Degraded"
		}
		rep.Devices = append(rep.Devices, storagev1alpha1.DeviceStatus{Path: pv.Name, State: state})
	}
	return rep, nil
}

// PoolReconciler runs on every node and acts only on pools whose spec.node
// matches; filtering happens here, not in the watch, so a mislabeled pool
// is visibly ignored rather than silently unwatched.
type PoolReconciler struct {
	client.Client
	Node string
	LVM  *lvm.LVM
}

// refreshInterval bounds how stale capacity and device health can get
// between spec-driven reconciles.
const refreshInterval = time.Minute

func (r *PoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool storagev1alpha1.StoragePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pool.Spec.Node != r.Node {
		return ctrl.Result{}, nil
	}

	rep, ensureErr := EnsurePool(ctx, r.LVM, &pool)

	pool.Status.VG = rep.VG
	pool.Status.Health = rep.Health
	pool.Status.Devices = rep.Devices
	if rep.Capacity > 0 {
		pool.Status.Capacity = resource.NewQuantity(rep.Capacity, resource.BinarySI)
		pool.Status.Free = resource.NewQuantity(rep.Free, resource.BinarySI)
	}

	cond := metav1.Condition{
		Type:               storagev1alpha1.ConditionHostReady,
		Status:             metav1.ConditionTrue,
		Reason:             storagev1alpha1.ReasonReady,
		ObservedGeneration: pool.Generation,
	}
	if ensureErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = storagev1alpha1.ReasonHostError
		cond.Message = ensureErr.Error()
	}
	meta.SetStatusCondition(&pool.Status.Conditions, cond)

	if err := r.Status().Update(ctx, &pool); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	if ensureErr != nil {
		return ctrl.Result{}, ensureErr
	}
	return ctrl.Result{RequeueAfter: refreshInterval}, nil
}

func (r *PoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.StoragePool{}).
		Named("pool-agent").
		Complete(r)
}
