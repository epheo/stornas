// Package agent converges host state to CRD specs. It is the dumb half of
// the split: all decisions live in the operator, the agent only executes
// and observes (CLAUDE.md invariants).
package agent

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/lvm"
	"github.com/epheo/stornas/internal/mdraid"
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

// EnsurePool converges host state for one pool and reports what it saw.
// It never deletes volumes or shrinks the pool: pool deletion stays a
// human decision. Membership does converge to spec.devices, which is how
// a swapped entry (the disk replace flow) reaches the host. Raid pools
// put mdadm below the PV so the thin pool stays linear (DESIGN.md).
func EnsurePool(ctx context.Context, l *lvm.LVM, md *mdraid.MD, pool *storagev1alpha1.StoragePool) (Report, error) {
	if pool.Spec.Raid == "" || pool.Spec.Raid == "none" {
		return ensureLinearPool(ctx, l, pool)
	}
	return ensureRaidPool(ctx, l, md, pool)
}

// TeardownPool wipes what EnsurePool built so the disks read unclaimed
// again: VG (with every LV), then the array and member signatures or
// the PV labels. Every step tolerates absence; teardown re-runs until
// the operator sees TornDown.
func TeardownPool(ctx context.Context, l *lvm.LVM, md *mdraid.MD, pool *storagev1alpha1.StoragePool) error {
	if err := l.VGRemove(ctx, pool.VGName()); err != nil {
		return err
	}
	if pool.Spec.Raid != "" && pool.Spec.Raid != "none" {
		if err := md.Stop(ctx, mdraid.DevPath(pool.Name)); err != nil {
			return err
		}
		for _, dev := range pool.Spec.Devices {
			if err := md.ZeroSuperblock(ctx, l.ResolvePath(ctx, dev)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, dev := range pool.Spec.Devices {
		if err := l.PVWipe(ctx, dev); err != nil {
			return err
		}
	}
	return nil
}

func ensureLinearPool(ctx context.Context, l *lvm.LVM, pool *storagev1alpha1.StoragePool) (Report, error) {
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
	if !l.IsThinPool(ctx, vg, storagev1alpha1.ThinLV) {
		if err := l.CreateThinPool(ctx, vg, storagev1alpha1.ThinLV); err != nil {
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
	// spec swaps in a real disk.
	live := plan.Add[:0]
	for _, dev := range plan.Add {
		if l.IsBlockDev(ctx, dev) {
			live = append(live, dev)
		}
	}
	plan.Add = live
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
	// pvs names a dead member "[unknown]"; pair it with the spec device
	// that lost its disk, or the replace flow cannot name the victim.
	presentSpec := map[string]bool{}
	for _, pv := range pvs {
		if sp, ok := specPath[pv.Name]; ok && !pv.Missing {
			presentSpec[sp] = true
		}
	}
	var orphaned []string
	for _, dev := range pool.Spec.Devices {
		if !presentSpec[dev] {
			orphaned = append(orphaned, dev)
		}
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
		} else if pv.Missing && len(orphaned) > 0 {
			path, orphaned = orphaned[0], orphaned[1:]
		}
		rep.Devices = append(rep.Devices, storagev1alpha1.DeviceStatus{Path: path, State: state})
	}
	return rep, nil
}

func ensureRaidPool(ctx context.Context, l *lvm.LVM, md *mdraid.MD, pool *storagev1alpha1.StoragePool) (Report, error) {
	vg := pool.VGName()
	rep := Report{VG: vg, Health: "Failed"}
	dev := mdraid.DevPath(pool.Name)

	if !md.Exists(ctx, dev) {
		if err := md.Create(ctx, dev, "stornas-"+pool.Name, pool.Spec.Raid, pool.Spec.Devices); err != nil {
			return rep, err
		}
	}
	if !l.VGExists(ctx, vg) {
		if !l.IsPV(ctx, dev) {
			if err := l.CreatePV(ctx, dev); err != nil {
				return rep, err
			}
		}
		if err := l.CreateVG(ctx, vg, []string{dev}); err != nil {
			return rep, err
		}
	}
	if !l.IsThinPool(ctx, vg, storagev1alpha1.ThinLV) {
		if err := l.CreateThinPool(ctx, vg, storagev1alpha1.ThinLV); err != nil {
			return rep, err
		}
	}

	// Best effort: detection aid only, Detail stays authoritative.
	if err := md.FailDetached(ctx, dev); err != nil {
		fmt.Printf("mdadm fail detached %s: %v\n", dev, err)
	}
	detail, err := md.Detail(ctx, dev)
	if err != nil {
		return rep, err
	}
	// Faulty and vanished members leave first so their slots free up for
	// the newcomer the replace flow put in the spec.
	for _, mb := range detail.Members {
		if mb.State == "Failed" {
			if err := md.RemoveFailed(ctx, dev); err != nil {
				return rep, err
			}
			if detail, err = md.Detail(ctx, dev); err != nil {
				return rep, err
			}
			break
		}
	}

	resolved := map[string]string{}
	for _, d := range pool.Spec.Devices {
		resolved[d] = l.ResolvePath(ctx, d)
	}
	members := map[string]bool{}
	inSpec := map[string]bool{}
	for _, mb := range detail.Members {
		if mb.Path != "" {
			members[mb.Path] = true
		}
	}
	for _, d := range pool.Spec.Devices {
		inSpec[resolved[d]] = true
	}
	for _, d := range pool.Spec.Devices {
		if members[resolved[d]] || !l.IsBlockDev(ctx, d) {
			continue
		}
		// A live member outside the spec means a healthy disk is being
		// swapped: --replace rebuilds onto the newcomer before failing
		// it, so redundancy never drops. Otherwise the newcomer fills a
		// freed slot.
		old := ""
		for _, mb := range detail.Members {
			if mb.Path != "" && mb.State == "InSync" && !inSpec[mb.Path] {
				old = mb.Path
				break
			}
		}
		if old != "" {
			err = md.Replace(ctx, dev, old, d)
		} else {
			err = md.Add(ctx, dev, d)
		}
		if err != nil {
			return rep, err
		}
		if detail, err = md.Detail(ctx, dev); err != nil {
			return rep, err
		}
	}

	info, err := l.VGInfo(ctx, vg)
	if err != nil {
		return rep, err
	}
	rep.Capacity = info.SizeBytes
	rep.Free = info.FreeBytes
	rep.Rebuild = detail.SyncPercent
	rep.Health = "Online"
	if detail.Degraded {
		rep.Health = "Degraded"
	}

	// Slots whose disk is gone carry no kernel path (and a pulled disk's
	// by-id no longer resolves); pair them with the spec devices that
	// lost their disk so the replace flow can name the victim.
	kernelToSpec := map[string]string{}
	for d, r := range resolved {
		kernelToSpec[r] = d
	}
	presentSpec := map[string]bool{}
	for _, mb := range detail.Members {
		if sp, ok := kernelToSpec[mb.Path]; ok && mb.Path != "" {
			presentSpec[sp] = true
		}
	}
	var orphaned []string
	for _, d := range pool.Spec.Devices {
		if !presentSpec[d] {
			orphaned = append(orphaned, d)
		}
	}
	for _, mb := range detail.Members {
		path := mb.Path
		if sp, ok := kernelToSpec[mb.Path]; ok && mb.Path != "" {
			path = sp
		} else if mb.State != "InSync" && len(orphaned) > 0 {
			path, orphaned = orphaned[0], orphaned[1:]
		}
		rep.Devices = append(rep.Devices, storagev1alpha1.DeviceStatus{Path: path, State: mb.State})
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
	MD   *mdraid.MD
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
	if pool.DeletionTimestamp != nil {
		// The operator's finalizer holds the CR until TornDown: the spec
		// (devices, raid level) is the only map of what to wipe.
		if meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionTornDown) {
			return ctrl.Result{}, nil
		}
		if err := TeardownPool(ctx, r.LVM, r.MD, &pool); err != nil {
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionTornDown,
			Status:             metav1.ConditionTrue,
			Reason:             storagev1alpha1.ReasonReady,
			ObservedGeneration: pool.Generation,
		})
		if err := r.Status().Update(ctx, &pool); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	rep, ensureErr := EnsurePool(ctx, r.LVM, r.MD, &pool)

	if r.Smart != nil {
		for i := range rep.Devices {
			if info, ok := r.Smart.Get(rep.Devices[i].Path); ok {
				rep.Devices[i].Smart = info.Verdict
			}
		}
	}

	pool.Status.VG = rep.VG
	pool.Status.Health = rep.Health
	// Keep last-known devices through a failed pass: a transient LVM error
	// must not blank the degraded view it is supposed to explain.
	if len(rep.Devices) > 0 {
		pool.Status.Devices = rep.Devices
		pool.Status.RebuildPercent = rep.Rebuild
	}
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
