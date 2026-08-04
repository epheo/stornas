package agent

import (
	"context"
	"testing"
)

func TestCollectPrefersWWNAndMarksClaimed(t *testing.T) {
	lsblk := `{"blockdevices":[
	  {"path":"/dev/sda","model":"WD_RED","serial":"S1","size":4000000000000,"rota":true,"type":"disk","wwn":"0x5000c500a1b2c3d4"},
	  {"path":"/dev/sdb","model":"SSD","serial":"S2","size":500000000000,"rota":false,"type":"disk","wwn":""},
	  {"path":"/dev/sr0","model":"DVD","serial":"","size":0,"rota":true,"type":"rom","wwn":""}
	]}`
	f := &fakeRunner{results: map[string]result{
		"lsblk --json -b -d -o PATH,MODEL,SERIAL,SIZE,ROTA,TYPE,WWN": {out: lsblk},
		"pvs --noheadings -o pv_name":                                {out: "  /dev/sdb\n"},
	}}
	p := &InventoryPublisher{Run: f, Node: "node-a"}

	disks, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 {
		t.Fatalf("disks = %+v", disks)
	}
	if disks[0].Path != "/dev/disk/by-id/wwn-0x5000c500a1b2c3d4" || disks[0].Claimed {
		t.Fatalf("disk0 = %+v", disks[0])
	}
	if disks[1].Path != "/dev/sdb" || !disks[1].Claimed || disks[1].Rotational {
		t.Fatalf("disk1 = %+v", disks[1])
	}
}
