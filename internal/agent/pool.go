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
	// Rebuild is the lowest raid-sync/pvmove completion, nil when idle.
	Rebuild *int32
}

// EnsurePool converges LVM state for one pool and reports what it saw.
// It never deletes volumes or shrinks the pool: pool deletion stays a
// human decision. Membership does converge to spec.devices, which is how
// a swapped entry (the disk replace flow) reaches the host.
func EnsurePool(ctx context.Context, l *lvm.LVM, pool *storagev1alpha1.StoragePool) (Report, error) {
	vg := pool.VGName()
	rep := Report{VG: vg, Health: "Failed"}

	if !l.VGExists(ctx, vg) {
		for _, dev := range pool.Spec.Devices {
			if !l.IsPV(ctx, dev) {
				if err := l.CreatePV(ctx, dev); err != nil {
					return rep, err
				}
			}
		}
		if err := l.CreateVG(ctx, vg, pool.Spec.Devices); err != nil {
			return rep, err
		}
	}
	if !l.LVExists(ctx, vg, storagev1alpha1.ThinLV) {
		if err := l.CreateThinPool(ctx, vg, storagev1alpha1.ThinLV, pool.Spec.Raid); err != nil {
			return rep, err
		}
	}

	pvs, err := l.PVs(ctx, vg)
	if err != nil {
		return rep, err
	}
	resolved := map[string]string{}
	for _, dev := range pool.Spec.Devices {
		resolved[dev] = l.ResolvePath(ctx, dev)
	}
	plan := planDevices(pool.Spec.Devices, resolved, pvs)
	// A dead member still in the spec plans as an add; a device absent
	// from the host cannot join, so it stays a Missing report until the
	// spec swaps in a real disk. Repair likewise waits for one.
	live := plan.Add[:0]
	for _, dev := range plan.Add {
		if l.IsBlockDev(ctx, dev) {
			live = append(live, dev)
		}
	}
	plan.Add = live
	if len(plan.Add) == 0 {
		plan.Missing = false
	}
	added := map[string]bool{}
	if !plan.empty() {
		if err := convergeDevices(ctx, l, pool, plan); err != nil {
			return rep, err
		}
		for _, dev := range plan.Add {
			added[resolved[dev]] = true
		}
		if pvs, err = l.PVs(ctx, vg); err != nil {
			return rep, err
		}
	}

	info, err := l.VGInfo(ctx, vg)
	if err != nil {
		return rep, err
	}
	rep.Rebuild, err = l.SyncPercent(ctx, vg)
	if err != nil {
		return rep, err
	}

	rep.Capacity = info.SizeBytes
	rep.Free = info.FreeBytes
	rep.Health = "Online"
	// Report under the spec's path (often by-id) rather than the kernel
	// path pvs prints, so status joins back onto spec.devices.
	specPath := map[string]string{}
	for dev, r := range resolved {
		specPath[r] = dev
	}
	for _, pv := range pvs {
		state := "InSync"
		switch {
		case pv.Missing:
			state = "Missing"
			rep.Health = "Degraded"
		case rep.Rebuild != nil && added[pv.Name]:
			state = "Rebuilding"
		}
		path := pv.Name
		if sp, ok := specPath[pv.Name]; ok {
			path = sp
		}
		rep.Devices = append(rep.Devices, storagev1alpha1.DeviceStatus{Path: path, State: state})
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
	// Smart joins the inventory sweep's verdicts onto device status.
	Smart *SmartStore
}

// refreshInterval bounds how stale capacity and device health can get
// between spec-driven reconciles.
const refreshInterval = time.Minute

// rebuildInterval paces status while a repair or evacuation runs.
const rebuildInterval = 10 * time.Second

func (r *PoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool storagev1alpha1.StoragePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pool.Spec.Node != r.Node {
		return ctrl.Result{}, nil
	}

	rep, ensureErr := EnsurePool(ctx, r.LVM, &pool)

	if r.Smart != nil {
		for i := range rep.Devices {
			if info, ok := r.Smart.Get(rep.Devices[i].Path); ok {
				rep.Devices[i].Smart = info.Verdict
			}
		}
	}

	pool.Status.VG = rep.VG
	pool.Status.Health = rep.Health
	pool.Status.Devices = rep.Devices
	pool.Status.RebuildPercent = rep.Rebuild
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
	if rep.Rebuild != nil {
		// A live rebuild deserves live progress.
		return ctrl.Result{RequeueAfter: rebuildInterval}, nil
	}
	return ctrl.Result{RequeueAfter: refreshInterval}, nil
}

func (r *PoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.StoragePool{}).
		Named("pool-agent").
		Complete(r)
}
