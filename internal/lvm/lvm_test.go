package lvm

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeRunner struct {
	// results keyed by the full command line; a missing key is a test bug.
	results map[string]result
	calls   []string
}

type result struct {
	out string
	err error
}

var errExit = fmt.Errorf("exit status 5")

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, cmd)
	r, ok := f.results[cmd]
	if !ok {
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}
	return []byte(r.out), r.err
}

func TestIsThinPool(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"lvs --noheadings --options lv_attr vg0/thin": {out: "  twi-aotz--\n"},
		"lvs --noheadings --options lv_attr vg0/bare": {out: "  rwi-a-r---\n"},
		"lvs --noheadings --options lv_attr vg0/gone": {err: errExit},
	}}
	l := NewWithRunner(f)
	if !l.IsThinPool(context.Background(), "vg0", "thin") {
		t.Fatal("thin pool not recognized")
	}
	if l.IsThinPool(context.Background(), "vg0", "bare") {
		t.Fatal("bare raid LV mistaken for a thin pool")
	}
	if l.IsThinPool(context.Background(), "vg0", "gone") {
		t.Fatal("missing LV mistaken for a thin pool")
	}
}

func TestCreateThinPool(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"lvcreate --type thin-pool --extents 90%VG --name thin vg0": {},
	}}
	if err := NewWithRunner(f).CreateThinPool(context.Background(), "vg0", "thin"); err != nil {
		t.Fatal(err)
	}
}

func TestVGInfo(t *testing.T) {
	out := `{"report":[{"vg":[{"vg_size":"4000000000","vg_free":"1000000000"}]}]}`
	f := &fakeRunner{results: map[string]result{
		"vgs --reportformat json --units b --nosuffix --options vg_size,vg_free vg0": {out: out},
	}}
	info, err := NewWithRunner(f).VGInfo(context.Background(), "vg0")
	if err != nil {
		t.Fatal(err)
	}
	if info.SizeBytes != 4000000000 || info.FreeBytes != 1000000000 {
		t.Fatalf("info = %+v", info)
	}
}

func TestPVsMissing(t *testing.T) {
	out := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":"","pv_used":"1024"},{"pv_name":"/dev/sdb","pv_missing":"missing","pv_used":"0"}]}]}`
	f := &fakeRunner{results: map[string]result{
		"pvs --reportformat json --units b --nosuffix --options pv_name,pv_missing,pv_used --select vg_name=vg0": {out: out},
	}}
	pvs, err := NewWithRunner(f).PVs(context.Background(), "vg0")
	if err != nil {
		t.Fatal(err)
	}
	if len(pvs) != 2 || pvs[0].Missing || !pvs[1].Missing {
		t.Fatalf("pvs = %+v", pvs)
	}
	if pvs[0].UsedBytes != 1024 {
		t.Fatalf("used = %d", pvs[0].UsedBytes)
	}
}

func TestSyncPercentLowestWins(t *testing.T) {
	out := `{"report":[{"lv":[{"lv_name":"thin_tdata","sync_percent":"62.10","copy_percent":""},{"lv_name":"[pvmove0]","sync_percent":"","copy_percent":"41.90"},{"lv_name":"thin","sync_percent":"100.00","copy_percent":""}]}]}`
	f := &fakeRunner{results: map[string]result{
		"lvs --reportformat json -a --options lv_name,sync_percent,copy_percent vg0": {out: out},
	}}
	pct, err := NewWithRunner(f).SyncPercent(context.Background(), "vg0")
	if err != nil {
		t.Fatal(err)
	}
	if pct == nil || *pct != 41 {
		t.Fatalf("pct = %v", pct)
	}
}

func TestSyncPercentIdle(t *testing.T) {
	out := `{"report":[{"lv":[{"lv_name":"thin","sync_percent":"","copy_percent":""}]}]}`
	f := &fakeRunner{results: map[string]result{
		"lvs --reportformat json -a --options lv_name,sync_percent,copy_percent vg0": {out: out},
	}}
	pct, err := NewWithRunner(f).SyncPercent(context.Background(), "vg0")
	if err != nil {
		t.Fatal(err)
	}
	if pct != nil {
		t.Fatalf("pct = %v", *pct)
	}
}

func TestProbesUseExitCode(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"pvs /dev/sda": {err: errExit},
		"vgs vg0":      {},
	}}
	l := NewWithRunner(f)
	if l.IsPV(context.Background(), "/dev/sda") {
		t.Fatal("IsPV should be false on nonzero exit")
	}
	if !l.VGExists(context.Background(), "vg0") {
		t.Fatal("VGExists should be true on zero exit")
	}
}
