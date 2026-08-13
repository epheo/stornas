package agent

import (
	"context"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/lvm"
)

// devicePlan is what one convergence pass must do so VG membership
// matches spec.devices (the disk replace flow: the spec swapped a
// member, the host still runs the old set).
type devicePlan struct {
	// Add: spec devices not yet VG members.
	Add []string
	// Evacuate: members outside the spec still holding extents.
	Evacuate []string
	// Drop: members outside the spec with nothing allocated.
	Drop []string
	// Missing: a member's disk is gone from the host.
	Missing bool
}

func (p devicePlan) empty() bool {
	return len(p.Add) == 0 && len(p.Evacuate) == 0 && len(p.Drop) == 0 && !p.Missing
}

// planDevices compares the spec (resolved to kernel paths, pvs reports
// those) against observed VG members.
func planDevices(spec []string, resolved map[string]string, pvs []lvm.PV) devicePlan {
	var plan devicePlan
	want := map[string]bool{}
	for _, dev := range spec {
		r := resolved[dev]
		if r == "" {
			r = dev
		}
		want[r] = true
	}
	present := map[string]bool{}
	for _, pv := range pvs {
		if pv.Missing {
			plan.Missing = true
			continue
		}
		present[pv.Name] = true
		if want[pv.Name] {
			continue
		}
		if pv.UsedBytes > 0 {
			plan.Evacuate = append(plan.Evacuate, pv.Name)
		} else {
			plan.Drop = append(plan.Drop, pv.Name)
		}
	}
	for _, dev := range spec {
		r := resolved[dev]
		if r == "" {
			r = dev
		}
		if !present[r] {
			plan.Add = append(plan.Add, dev)
		}
	}
	return plan
}

// convergeDevices executes one plan step set for a linear pool; raid
// membership converges through mdadm instead (ensureRaidPool). Order
// matters: the replacement joins first so pvmove has somewhere to go.
// A missing member here lost data, and only deleting the pool is honest,
// so there is no repair step.
func convergeDevices(ctx context.Context, l *lvm.LVM, pool *storagev1alpha1.StoragePool, plan devicePlan) error {
	vg := pool.VGName()
	for _, dev := range plan.Add {
		if !l.IsPV(ctx, dev) {
			if err := l.CreatePV(ctx, dev); err != nil {
				return err
			}
		}
		if err := l.VGExtend(ctx, vg, dev); err != nil {
			return err
		}
	}
	// Direct evacuation onto the fresh member when there is exactly one;
	// otherwise the allocator picks.
	dst := ""
	if len(plan.Add) == 1 {
		dst = plan.Add[0]
	}
	for _, dev := range plan.Evacuate {
		if err := l.PVMove(ctx, dev, dst); err != nil {
			return err
		}
	}
	for _, dev := range plan.Drop {
		if err := l.VGReduce(ctx, vg, dev); err != nil {
			return err
		}
		if err := l.PVRemove(ctx, dev); err != nil {
			return err
		}
	}
	return nil
}
