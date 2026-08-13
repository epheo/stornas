package agent

import (
	"context"
	"testing"
)

// The by-id alias must win over the kernel path, wwn over other aliases,
// and a disk with no alias keeps its kernel path.
func TestCollectPrefersStableAliasesAndMarksClaimed(t *testing.T) {
	lsblk := `{"blockdevices":[
	  {"path":"/dev/sda","model":"WD_RED","serial":"S1","size":4000000000000,"rota":true,"type":"disk"},
	  {"path":"/dev/vdb","model":"","serial":"STORNASB","size":500000000000,"rota":false,"type":"disk"},
	  {"path":"/dev/vdc","model":"","serial":"","size":500000000000,"rota":false,"type":"disk"},
	  {"path":"/dev/sr0","model":"DVD","serial":"","size":0,"rota":true,"type":"rom"}
	]}`
	byid := "ata-WD_RED_S1 ../../sda\n" +
		"wwn-0x5000c500a1b2c3d4 ../../sda\n" +
		"virtio-STORNASB ../../vdb\n" +
		"lvm-pv-uuid-abcdef ../../vdb\n" +
		"virtio-STORNASB-part1 ../../vdb1\n"
	f := &fakeRunner{results: map[string]result{
		"lsblk --json -b -d -o PATH,MODEL,SERIAL,SIZE,ROTA,TYPE":   {out: lsblk},
		"find /dev/disk/by-id -maxdepth 1 -type l -printf %f %l\n": {out: byid},
		"pvs --noheadings -o pv_name":                              {out: "  /dev/vdb\n"},
	}}
	p := &InventoryPublisher{Run: f, Node: "node-a"}

	disks, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 3 {
		t.Fatalf("disks = %+v", disks)
	}
	if disks[0].Path != "/dev/disk/by-id/wwn-0x5000c500a1b2c3d4" || disks[0].Claimed {
		t.Fatalf("disk0 = %+v", disks[0])
	}
	if disks[1].Path != "/dev/disk/by-id/virtio-STORNASB" || !disks[1].Claimed {
		t.Fatalf("disk1 = %+v", disks[1])
	}
	if disks[2].Path != "/dev/vdc" {
		t.Fatalf("disk2 = %+v", disks[2])
	}
}
