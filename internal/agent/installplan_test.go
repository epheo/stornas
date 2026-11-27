package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

func TestReadPlan(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "data-disks.json")
	if pools, layout, err := readPlan(p); err != nil || pools != nil || layout != "" {
		t.Fatalf("no file: %v %q %v", pools, layout, err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"version":1,"layout":"pairs","pools":[{"name":"hdd-1","raid":"raid1","tier":"hdd","devices":["/dev/disk/by-id/a","/dev/disk/by-id/b"]}]}`)
	pools, layout, err := readPlan(p)
	if err != nil || layout != "pairs" || len(pools) != 1 || pools[0].Name != "hdd-1" || len(pools[0].Devices) != 2 || pools[0].Raid != "raid1" {
		t.Fatalf("got %+v %q %v", pools, layout, err)
	}
	// Another image's plan is reported as it is; the operator decides.
	write(`{"version":1,"layout":"thin","pools":[{"name":"data","raid":"none","devices":["/dev/disk/by-id/a"]}]}`)
	if pools, layout, err := readPlan(p); err != nil || layout != "thin" || len(pools) != 1 {
		t.Fatalf("thin: %+v %q %v", pools, layout, err)
	}
	// A pool the inventory's schema would refuse is left out, the rest kept.
	write(`{"version":1,"layout":"pairs","pools":[{"name":"bad","raid":"raid10","devices":["/dev/disk/by-id/a"]},` +
		`{"name":"empty","raid":"raid1","devices":[]},{"name":"odd","raid":"raid1","tier":"ssd_fast","devices":["/dev/disk/by-id/d"]},` +
		`{"name":"ok","raid":"raid1","devices":["/dev/disk/by-id/c"]}]}`)
	if pools, _, err := readPlan(p); err == nil || len(pools) != 1 || pools[0].Name != "ok" {
		t.Fatalf("bad pools: %+v %v", pools, err)
	}
	write(`{"version":2}`)
	if _, _, err := readPlan(p); err == nil {
		t.Fatal("an unknown version was accepted")
	}
}

// The installer may name a disk by another of its links than the inventory
// does; the plan is reported under the inventory's names.
func TestCanonicalPlanDevices(t *testing.T) {
	find := "find /dev/disk/by-id -maxdepth 1 -type l -printf %f %l\n"
	r := &fakeRunner{results: map[string]result{find: {out: "nvme-eui.0025 ../../nvme0n1\n" +
		"nvme-Samsung_SSD_S6P ../../nvme0n1\nwwn-0x5000c5 ../../sda\nata-ST8000_ZA1 ../../sda\n"}}}
	p := &InventoryPublisher{Run: r}
	got := p.canonical(context.Background(), []storagev1alpha1.PlannedPool{{Name: "ssd-1", Raid: "raid1",
		Devices: []string{"/dev/disk/by-id/nvme-eui.0025", "/dev/disk/by-id/ata-ST8000_ZA1", "/dev/disk/by-id/gone"}}})
	want := []string{"/dev/disk/by-id/nvme-Samsung_SSD_S6P", "/dev/disk/by-id/wwn-0x5000c5", "/dev/disk/by-id/gone"}
	for i, d := range got[0].Devices {
		if d != want[i] {
			t.Fatalf("devices %v, want %v", got[0].Devices, want)
		}
	}
}
