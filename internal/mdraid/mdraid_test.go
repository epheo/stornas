package mdraid

import (
	"context"
	"fmt"
	"testing"
)

type result struct {
	out string
	err error
}

type fakeRunner struct {
	results map[string]result
	calls   []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := name
	for _, a := range args {
		cmd += " " + a
	}
	f.calls = append(f.calls, cmd)
	if r, ok := f.results[cmd]; ok {
		return []byte(r.out), r.err
	}
	return nil, fmt.Errorf("unexpected command: %s", cmd)
}

const detailHealthy = `/dev/md/stornas-test:
           Version : 1.2
        Raid Level : raid1
      Raid Devices : 2
             State : clean
    Active Devices : 2

    Number   Major   Minor   RaidDevice State
       0     252       16        0      active sync   /dev/vdb
       1     252       32        1      active sync   /dev/vdc
`

const detailDegraded = `/dev/md/stornas-test:
        Raid Level : raid1
             State : clean, degraded
    Active Devices : 1

    Number   Major   Minor   RaidDevice State
       -       0        0        0      removed
       1     252       32        1      active sync   /dev/vdc
`

const detailRebuilding = `/dev/md/stornas-test:
        Raid Level : raid1
             State : clean, degraded, recovering
    Rebuild Status : 43% complete

    Number   Major   Minor   RaidDevice State
       2     252       48        0      spare rebuilding   /dev/vdd
       1     252       32        1      active sync   /dev/vdc
`

func TestParseDetailHealthy(t *testing.T) {
	d := parseDetail(detailHealthy)
	if d.Degraded || d.SyncPercent != nil {
		t.Fatalf("detail = %+v", d)
	}
	if len(d.Members) != 2 || d.Members[0].Path != "/dev/vdb" || d.Members[0].State != "InSync" {
		t.Fatalf("members = %+v", d.Members)
	}
}

func TestParseDetailDegraded(t *testing.T) {
	d := parseDetail(detailDegraded)
	if !d.Degraded {
		t.Fatal("want degraded")
	}
	if len(d.Members) != 2 || d.Members[0].Path != "" || d.Members[0].State != "Missing" {
		t.Fatalf("members = %+v", d.Members)
	}
	if d.Members[1].State != "InSync" {
		t.Fatalf("members = %+v", d.Members)
	}
}

func TestParseDetailRebuilding(t *testing.T) {
	d := parseDetail(detailRebuilding)
	if d.SyncPercent == nil || *d.SyncPercent != 43 {
		t.Fatalf("sync = %v", d.SyncPercent)
	}
	if d.Members[0].State != "Rebuilding" || d.Members[0].Path != "/dev/vdd" {
		t.Fatalf("members = %+v", d.Members)
	}
}

func TestMembersFromMdstat(t *testing.T) {
	mdstat := `Personalities : [raid1]
md127 : active raid1 vdc[1] vdb[0](F)
      10475520 blocks super 1.2 [2/1] [_U]

unused devices: <none>
`
	f := &fakeRunner{results: map[string]result{
		"cat /proc/mdstat": {out: mdstat},
	}}
	got := NewWithRunner(f).Members(context.Background())
	if !got["/dev/vdb"] || !got["/dev/vdc"] || len(got) != 2 {
		t.Fatalf("members = %v", got)
	}
}

func TestReplaceAddsSpareFirst(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"mdadm /dev/md/stornas-test --add-spare /dev/vdd":               {},
		"mdadm /dev/md/stornas-test --replace /dev/vdb --with /dev/vdd": {},
	}}
	if err := NewWithRunner(f).Replace(context.Background(), "/dev/md/stornas-test", "/dev/vdb", "/dev/vdd"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[0] != "mdadm /dev/md/stornas-test --add-spare /dev/vdd" {
		t.Fatalf("calls = %v", f.calls)
	}
}
