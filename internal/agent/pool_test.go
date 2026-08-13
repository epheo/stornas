package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/lvm"
)

type fakeRunner struct {
	results map[string]result
	calls   []string
	stdins  map[string]string
}

type result struct {
	out string
	err error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, cmd)
	r, ok := f.results[cmd]
	if !ok {
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}
	return []byte(r.out), r.err
}

func (f *fakeRunner) RunInput(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	if f.stdins == nil {
		f.stdins = map[string]string{}
	}
	f.stdins[strings.Join(append([]string{name}, args...), " ")] = stdin
	return f.Run(ctx, name, args...)
}

func pool(name, raid string, devices ...string) *storagev1alpha1.StoragePool {
	return &storagev1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       storagev1alpha1.StoragePoolSpec{Node: "node-a", Devices: devices, Raid: raid},
	}
}

var errExit = fmt.Errorf("exit status 5")

const (
	vgsCmd     = "vgs --reportformat json --units b --nosuffix --options vg_size,vg_free stornas-tank"
	pvsCmd     = "pvs --reportformat json --units b --nosuffix --options pv_name,pv_missing,pv_used --select vg_name=stornas-tank"
	lvsSyncCmd = "lvs --reportformat json -a --options lv_name,sync_percent,copy_percent stornas-tank"
	vgsOut     = `{"report":[{"vg":[{"vg_size":"100","vg_free":"90"}]}]}`
	lvsIdle    = `{"report":[{"lv":[{"lv_name":"thin","sync_percent":"","copy_percent":""}]}]}`
)

func TestEnsurePoolFreshCreate(t *testing.T) {
	pvs := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":"","pv_used":"10"}]}]}`
	f := &fakeRunner{results: map[string]result{
		"vgs stornas-tank":               {err: errExit},
		"pvs /dev/sda":                   {err: errExit},
		"pvcreate /dev/sda":              {},
		"vgcreate stornas-tank /dev/sda": {},
		"lvs --noheadings --options lv_attr stornas-tank/thin":               {err: errExit},
		"lvcreate --type thin-pool --extents 90%VG --name thin stornas-tank": {},
		"readlink -f /dev/sda": {out: "/dev/sda\n"},
		vgsCmd:                 {out: vgsOut},
		pvsCmd:                 {out: pvs},
		lvsSyncCmd:             {out: lvsIdle},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), pool("tank", "none", "/dev/sda"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.VG != "stornas-tank" || rep.Health != "Online" || rep.Capacity != 100 || rep.Free != 90 {
		t.Fatalf("rep = %+v", rep)
	}
	if len(rep.Devices) != 1 || rep.Devices[0].State != "InSync" {
		t.Fatalf("devices = %+v", rep.Devices)
	}
	if rep.Rebuild != nil {
		t.Fatalf("rebuild = %v", *rep.Rebuild)
	}
}

func TestEnsurePoolIdempotent(t *testing.T) {
	pvs := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":"","pv_used":"10"},{"pv_name":"/dev/sdb","pv_missing":"","pv_used":"10"}]}]}`
	f := &fakeRunner{results: map[string]result{
		"vgs stornas-tank": {},
		"lvs --noheadings --options lv_attr stornas-tank/thin": {out: "  twi-aotz--\n"},
		"readlink -f /dev/sda":                                 {out: "/dev/sda\n"},
		"readlink -f /dev/sdb":                                 {out: "/dev/sdb\n"},
		vgsCmd:                                                 {out: vgsOut},
		pvsCmd:                                                 {out: pvs},
		lvsSyncCmd:                                             {out: lvsIdle},
	}}

	if _, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), pool("tank", "raid1", "/dev/sda", "/dev/sdb")); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pvcreate") || strings.HasPrefix(c, "vgcreate") || strings.HasPrefix(c, "lvcreate") {
			t.Fatalf("existing pool must not be recreated: %s", c)
		}
	}
}

// A dead member with no replacement yet stays a Missing report; nothing
// tries to re-add the corpse or repair without a target.
func TestEnsurePoolDegradedOnMissingPV(t *testing.T) {
	pvs := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":"","pv_used":"10"},{"pv_name":"[unknown]","pv_missing":"missing","pv_used":"10"}]}]}`
	f := &fakeRunner{results: map[string]result{
		"vgs stornas-tank": {},
		"lvs --noheadings --options lv_attr stornas-tank/thin": {out: "  twi-aotz--\n"},
		"readlink -f /dev/sda":                                 {out: "/dev/sda\n"},
		"readlink -f /dev/sdb":                                 {out: "/dev/sdb\n"},
		"test -b /dev/sdb":                                     {err: errExit},
		vgsCmd:                                                 {out: vgsOut},
		pvsCmd:                                                 {out: pvs},
		lvsSyncCmd:                                             {out: lvsIdle},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), pool("tank", "raid1", "/dev/sda", "/dev/sdb"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Health != "Degraded" {
		t.Fatalf("health = %s", rep.Health)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "lvconvert") || strings.HasPrefix(c, "vgreduce") || strings.HasPrefix(c, "pvcreate") {
			t.Fatalf("no replacement present, must not act: %s", c)
		}
	}
}

// The replace flow: spec swapped the dead sdb for sdc. The new disk
// joins, the raid legs repair, the ghost leaves, and the new member
// reports Rebuilding with pool-level progress.
func TestEnsurePoolRepairsWithReplacement(t *testing.T) {
	pvsBefore := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":"","pv_used":"10"},{"pv_name":"[unknown]","pv_missing":"missing","pv_used":"10"}]}]}`
	pvsAfter := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":"","pv_used":"10"},{"pv_name":"/dev/sdc","pv_missing":"","pv_used":"10"}]}]}`
	lvsSyncing := `{"report":[{"lv":[{"lv_name":"thin_tdata","sync_percent":"37.50","copy_percent":""}]}]}`
	f := &fakeRunner{results: map[string]result{
		"vgs stornas-tank": {},
		"lvs --noheadings --options lv_attr stornas-tank/thin": {out: "  twi-aotz--\n"},
		"readlink -f /dev/sda":                                 {out: "/dev/sda\n"},
		"readlink -f /dev/sdc":                                 {out: "/dev/sdc\n"},
		"test -b /dev/sdc":                                     {},
		"pvs /dev/sdc":                                         {err: errExit},
		"pvcreate /dev/sdc":                                    {},
		"vgextend stornas-tank /dev/sdc":                       {},
		"lvconvert --repair --yes stornas-tank/thin_tdata":     {},
		"lvconvert --repair --yes stornas-tank/thin_tmeta":     {},
		"vgreduce --removemissing stornas-tank":                {},
		vgsCmd:                                                 {out: vgsOut},
		lvsSyncCmd:                                             {out: lvsSyncing},
	}}
	// First pvs read sees the ghost, the re-read after convergence sees
	// the new member.
	first := true
	base := f.results
	f.results = nil
	runner := runnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		if cmd == pvsCmd {
			if first {
				first = false
				return []byte(pvsBefore), nil
			}
			return []byte(pvsAfter), nil
		}
		f.calls = append(f.calls, cmd)
		r, ok := base[cmd]
		if !ok {
			return nil, fmt.Errorf("unexpected command: %s", cmd)
		}
		return []byte(r.out), r.err
	})

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(runner), pool("tank", "raid1", "/dev/sda", "/dev/sdc"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Health != "Online" {
		t.Fatalf("health = %s", rep.Health)
	}
	if rep.Rebuild == nil || *rep.Rebuild != 37 {
		t.Fatalf("rebuild = %v", rep.Rebuild)
	}
	states := map[string]string{}
	for _, d := range rep.Devices {
		states[d.Path] = d.State
	}
	if states["/dev/sdc"] != "Rebuilding" || states["/dev/sda"] != "InSync" {
		t.Fatalf("devices = %+v", rep.Devices)
	}
}

type runnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestEnsurePoolFailsClosed(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"vgs stornas-tank":  {err: errExit},
		"pvs /dev/sda":      {err: errExit},
		"pvcreate /dev/sda": {err: errExit},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), pool("tank", "none", "/dev/sda"))
	if err == nil {
		t.Fatal("want error")
	}
	if rep.Health != "Failed" {
		t.Fatalf("health = %s", rep.Health)
	}
}

func errExitWith(msg string) error {
	return fmt.Errorf("exit status 1: %s", msg)
}
