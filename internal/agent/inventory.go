package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// InventoryPublisher refreshes this node's NodeInventory on a fixed tick:
// there is no event source for "a disk was plugged in", so discovery polls.
type InventoryPublisher struct {
	Client client.Client
	Node   string
	Run    Runner
}

const inventoryInterval = time.Minute

type lsblkReport struct {
	BlockDevices []struct {
		Path   string `json:"path"`
		Model  string `json:"model"`
		Serial string `json:"serial"`
		Size   int64  `json:"size"`
		Rota   bool   `json:"rota"`
		Type   string `json:"type"`
		WWN    string `json:"wwn"`
	} `json:"blockdevices"`
}

// Collect lists whole disks and marks the ones already carrying an LVM PV.
// The by-id WWN path is preferred: /dev/sdX names reshuffle across boots
// and StoragePool.spec.devices is immutable.
func (p *InventoryPublisher) Collect(ctx context.Context) ([]storagev1alpha1.Disk, error) {
	out, err := p.Run.Run(ctx, "lsblk", "--json", "-b", "-d",
		"-o", "PATH,MODEL,SERIAL,SIZE,ROTA,TYPE,WWN")
	if err != nil {
		return nil, err
	}
	var rep lsblkReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("parse lsblk: %w", err)
	}

	claimed := map[string]bool{}
	if pvout, err := p.Run.Run(ctx, "pvs", "--noheadings", "-o", "pv_name"); err == nil {
		for _, line := range splitLines(string(pvout)) {
			claimed[line] = true
		}
	}

	var disks []storagev1alpha1.Disk
	for _, d := range rep.BlockDevices {
		if d.Type != "disk" {
			continue
		}
		path := d.Path
		if d.WWN != "" {
			path = "/dev/disk/by-id/wwn-" + d.WWN
		}
		disks = append(disks, storagev1alpha1.Disk{
			Path:       path,
			Model:      d.Model,
			Serial:     d.Serial,
			Size:       resource.NewQuantity(d.Size, resource.BinarySI),
			Rotational: d.Rota,
			Claimed:    claimed[d.Path],
		})
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
	inv.Status.ObservedAt = metav1.Now()
	return p.Client.Status().Update(ctx, &inv)
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
