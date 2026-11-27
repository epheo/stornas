package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/mdraid"
)

// InventoryPublisher refreshes this node's NodeInventory on a fixed tick:
// there is no event source for "a disk was plugged in", so discovery polls.
type InventoryPublisher struct {
	Client client.Client
	Node   string
	Run    Runner
	// MD marks raid members claimed; they carry no PV, the array does.
	MD *mdraid.MD
	// Smart caches verdicts between sweeps and feeds the pool reconciler.
	Smart *SmartStore

	lastSweep time.Time
	// lastDisks and lastWrite gate the status write: an unconditional
	// ObservedAt bump made every tick an etcd write fanning out to every
	// watcher, the dominant write traffic on a quiet appliance.
	lastDisks []storagev1alpha1.Disk
	lastWrite time.Time
	// PlanPath is the installer's data disk plan on the host; the operator
	// turns it into pools once.
	PlanPath   string
	lastPlan   []storagev1alpha1.PlannedPool
	lastLayout string
}

// installPlan is the file the installer writes; version 1 only. Layout is
// the data-disks label of the image it was written for; the operator
// decides what to make of it.
type installPlan struct {
	Version int                           `json:"version"`
	Layout  string                        `json:"layout"`
	Pools   []storagev1alpha1.PlannedPool `json:"pools"`
}

// readPlan returns the installer's pools and layout, or none when there is
// no plan. A pool the inventory's schema would refuse is left out, so a
// hand-edited plan never stops the disks from being reported.
func readPlan(path string) ([]storagev1alpha1.PlannedPool, string, error) {
	if path == "" {
		return nil, "", nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var plan installPlan
	if err := json.Unmarshal(b, &plan); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if plan.Version != 1 {
		return nil, "", fmt.Errorf("%s: unknown version %d", path, plan.Version)
	}
	var pools []storagev1alpha1.PlannedPool
	var bad []string
	for _, p := range plan.Pools {
		if p.Name == "" || len(p.Devices) == 0 || (p.Raid != "none" && p.Raid != "raid1") {
			bad = append(bad, p.Name)
			continue
		}
		pools = append(pools, p)
	}
	if len(bad) > 0 {
		err = fmt.Errorf("%s: pools left out, not name, devices and raid none or raid1: %v", path, bad)
	}
	return pools, plan.Layout, err
}

// canonical names each planned disk as the inventory does: the installer
// may pick another of a disk's by-id links (nvme-eui before nvme-, say).
// A link that resolves to no disk stays as written; the operator skips it.
func (p *InventoryPublisher) canonical(ctx context.Context, pools []storagev1alpha1.PlannedPool) []storagev1alpha1.PlannedPool {
	out, err := p.Run.Run(ctx, "find", "/dev/disk/by-id", "-maxdepth", "1",
		"-type", "l", "-printf", "%f %l\n")
	if err != nil {
		return pools
	}
	kernel := map[string]string{}
	for _, line := range splitLines(string(out)) {
		if name, target, ok := strings.Cut(line, " "); ok {
			kernel["/dev/disk/by-id/"+name] = "/dev/" + target[strings.LastIndex(target, "/")+1:]
		}
	}
	aliases := p.byIDAliases(ctx)
	res := make([]storagev1alpha1.PlannedPool, len(pools))
	for i, pool := range pools {
		pool.Devices = slices.Clone(pool.Devices)
		for j, d := range pool.Devices {
			if a := aliases[kernel[d]]; a != "" {
				pool.Devices[j] = a
			}
		}
		res[i] = pool
	}
	return res
}

const inventoryInterval = time.Minute

// inventoryHeartbeat bounds ObservedAt staleness while disks are quiet;
// it is a human signal (printcolumn), not a control input.
const inventoryHeartbeat = 5 * time.Minute

// smartInterval spaces SMART sweeps out: health changes slowly and every
// query is a host command per disk.
const smartInterval = 5 * time.Minute

type lsblkReport struct {
	BlockDevices []struct {
		Path   string `json:"path"`
		Model  string `json:"model"`
		Serial string `json:"serial"`
		Size   int64  `json:"size"`
		Rota   bool   `json:"rota"`
		Type   string `json:"type"`
	} `json:"blockdevices"`
}

// byIDAliases maps kernel paths to a stable /dev/disk/by-id name: wwn
// wins, then the first alias alphabetically (virtio-, ata-, scsi-, ...).
// lvm-pv aliases churn with pool membership and never qualify. Best
// effort: on failure disks keep their kernel paths.
func (p *InventoryPublisher) byIDAliases(ctx context.Context) map[string]string {
	out, err := p.Run.Run(ctx, "find", "/dev/disk/by-id", "-maxdepth", "1",
		"-type", "l", "-printf", "%f %l\n")
	if err != nil {
		return nil
	}
	better := func(name, cur string) bool {
		if cur == "" {
			return true
		}
		nw, cw := strings.HasPrefix(name, "wwn-"), strings.HasPrefix(cur, "wwn-")
		if nw != cw {
			return nw
		}
		return name < cur
	}
	best := map[string]string{}
	for _, line := range splitLines(string(out)) {
		name, target, ok := strings.Cut(line, " ")
		if !ok || strings.HasPrefix(name, "lvm-pv-uuid-") {
			continue
		}
		kernel := "/dev/" + target[strings.LastIndex(target, "/")+1:]
		if better(name, best[kernel]) {
			best[kernel] = name
		}
	}
	aliases := make(map[string]string, len(best))
	for kernel, name := range best {
		aliases[kernel] = "/dev/disk/by-id/" + name
	}
	return aliases
}

// Collect lists whole disks and marks the ones already carrying an LVM PV.
// The by-id path is preferred: /dev/sdX names reshuffle across boots and
// StoragePool.spec.devices is immutable, so the UI must never offer a
// kernel path when a stable alias exists.
func (p *InventoryPublisher) Collect(ctx context.Context) ([]storagev1alpha1.Disk, error) {
	out, err := p.Run.Run(ctx, "lsblk", "--json", "-b", "-d",
		"-o", "PATH,MODEL,SERIAL,SIZE,ROTA,TYPE")
	if err != nil {
		return nil, err
	}
	var rep lsblkReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}

	// A failed pvs must abort the sweep: publishing without it would mark
	// every disk unclaimed and offer pool members to the create flow.
	pvout, err := p.Run.Run(ctx, "pvs", "--noheadings", "-o", "pv_name")
	if err != nil {
		return nil, err
	}
	claimed := map[string]bool{}
	for _, line := range splitLines(string(pvout)) {
		claimed[line] = true
	}
	if p.MD != nil {
		for dev := range p.MD.Members(ctx) {
			claimed[dev] = true
		}
	}

	sweep := p.Smart != nil && time.Since(p.lastSweep) >= smartInterval
	if sweep {
		p.lastSweep = time.Now()
	}

	aliases := p.byIDAliases(ctx)
	var disks []storagev1alpha1.Disk
	for _, d := range rep.BlockDevices {
		if d.Type != "disk" {
			continue
		}
		path := d.Path
		if a := aliases[d.Path]; a != "" {
			path = a
		}
		disk := storagev1alpha1.Disk{
			Path:       path,
			Model:      d.Model,
			Serial:     d.Serial,
			Size:       resource.NewQuantity(d.Size, resource.BinarySI),
			Rotational: d.Rota,
			Claimed:    claimed[d.Path],
		}
		if p.Smart != nil {
			if sweep {
				info := CheckSmart(ctx, p.Run, d.Path)
				if info.Verdict != "Unknown" {
					// Unknown often means standby (-n); keep the cache.
					p.Smart.Put(info, d.Path, path)
				} else if _, ok := p.Smart.Get(d.Path); !ok {
					p.Smart.Put(info, d.Path, path)
				}
			}
			if info, ok := p.Smart.Get(d.Path); ok {
				disk.Smart = info.Verdict
				disk.TempCelsius = info.TempCelsius
				disk.PowerOnHours = info.PowerOnHours
			}
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

// Start runs the publish loop; wired via mgr.Add so the cached client is
// ready before the first tick.
func (p *InventoryPublisher) Start(ctx context.Context) error {
	tick := time.NewTicker(inventoryInterval)
	defer tick.Stop()
	for {
		if err := p.publish(ctx); err != nil {
			// Next tick retries; inventory staleness is visible via observedAt.
			fmt.Printf("inventory publish: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (p *InventoryPublisher) publish(ctx context.Context) error {
	disks, err := p.Collect(ctx)
	if err != nil {
		return err
	}
	plan, layout, err := readPlan(p.PlanPath)
	if err != nil {
		// A bad plan never blocks the inventory; the pools can be made by hand.
		fmt.Printf("install plan: %v\n", err)
	}
	if len(plan) > 0 {
		plan = p.canonical(ctx, plan)
	}
	if reflect.DeepEqual(disks, p.lastDisks) && reflect.DeepEqual(plan, p.lastPlan) && layout == p.lastLayout &&
		time.Since(p.lastWrite) < inventoryHeartbeat {
		return nil
	}
	var inv storagev1alpha1.NodeInventory
	if err := p.Client.Get(ctx, types.NamespacedName{Name: p.Node}, &inv); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		inv = storagev1alpha1.NodeInventory{ObjectMeta: metav1.ObjectMeta{Name: p.Node}}
		if err := p.Client.Create(ctx, &inv); err != nil {
			return err
		}
	}
	inv.Status.Disks = disks
	inv.Status.InstallPlan = plan
	inv.Status.InstallPlanLayout = layout
	inv.Status.ObservedAt = metav1.Now()
	if err := p.Client.Status().Update(ctx, &inv); err != nil {
		return err
	}
	p.lastDisks, p.lastPlan, p.lastLayout, p.lastWrite = disks, plan, layout, time.Now()
	return nil
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}
