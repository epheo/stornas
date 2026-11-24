package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPlan(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "data-disks.json")
	if pools, err := readPlan(p); err != nil || pools != nil {
		t.Fatalf("no file: %v %v", pools, err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"version":1,"pools":[{"name":"hdd-1","raid":"raid1","tier":"hdd","devices":["/dev/disk/by-id/a","/dev/disk/by-id/b"]}]}`)
	pools, err := readPlan(p)
	if err != nil || len(pools) != 1 || pools[0].Name != "hdd-1" || len(pools[0].Devices) != 2 || pools[0].Raid != "raid1" {
		t.Fatalf("got %+v %v", pools, err)
	}
	write(`{"version":2}`)
	if _, err := readPlan(p); err == nil {
		t.Fatal("an unknown version was accepted")
	}
}
