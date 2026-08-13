package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/lvm"
	"github.com/epheo/stornas/internal/mdraid"
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

// seqRunner pops one canned result per invocation of the same command,
// for flows that read the same state twice (mdadm --detail before and
// after a convergence step).
type seqRunner struct {
	seq   map[string][]result
	calls []string
}

func (s *seqRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	s.calls = append(s.calls, cmd)
	q := s.seq[cmd]
	if len(q) == 0 {
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}
	r := q[0]
	s.seq[cmd] = q[1:]
	return []byte(r.out), r.err
}

func (s *seqRunner) RunInput(ctx context.Context, _ string, name string, args ...string) ([]byte, error) {
	return s.Run(ctx, name, args...)
}

func mdOf(f Runner) *mdraid.MD { return mdraid.NewWithRunner(f) }

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

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "none", "/dev/sda"))
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

// A dead member with no replacement yet stays a Missing report named by
// its spec path (pvs prints "[unknown]"); nothing tries to re-add the
// corpse. Linear pools have no repair: the data is gone.
func TestEnsurePoolLinearMissingNamesVictim(t *testing.T) {
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

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "none", "/dev/sda", "/dev/sdb"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Health != "Degraded" {
		t.Fatalf("health = %s", rep.Health)
	}
	missing := ""
	for _, d := range rep.Devices {
		if d.State == "Missing" {
			missing = d.Path
		}
	}
	if missing != "/dev/sdb" {
		t.Fatalf("missing device reported as %q, want /dev/sdb", missing)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "lvconvert") || strings.HasPrefix(c, "vgreduce") || strings.HasPrefix(c, "pvcreate") {
			t.Fatalf("no replacement present, must not act: %s", c)
		}
	}
}

const (
	mdDetailCmd = "mdadm --detail /dev/md/stornas-tank"
	mdVgsCmd    = vgsCmd
	mdHealthy   = `/dev/md/stornas-tank:
        Raid Level : raid1
             State : clean

    Number   Major   Minor   RaidDevice State
       0     252       16        0      active sync   /dev/sda
       1     252       32        1      active sync   /dev/sdb
`
	mdDegraded = `/dev/md/stornas-tank:
        Raid Level : raid1
             State : clean, degraded

    Number   Major   Minor   RaidDevice State
       0     252       16        0      active sync   /dev/sda
       -       0        0        1      removed
`
	mdRebuilding = `/dev/md/stornas-tank:
        Raid Level : raid1
             State : clean, degraded, recovering
    Rebuild Status : 43% complete

    Number   Major   Minor   RaidDevice State
       0     252       16        0      active sync   /dev/sda
       2     252       48        1      spare rebuilding   /dev/sdc
`
)

func TestEnsureRaidPoolFreshCreate(t *testing.T) {
	f := &seqRunner{seq: map[string][]result{
		mdDetailCmd: {{err: errExit}, {out: mdHealthy}},
		"mdadm --create /dev/md/stornas-tank --run --force --name=stornas-tank --homehost=stornas --level=raid1 --raid-devices=2 /dev/sda /dev/sdb": {{}},
		"vgs stornas-tank":                                                   {{err: errExit}},
		"pvs /dev/md/stornas-tank":                                           {{err: errExit}},
		"pvcreate /dev/md/stornas-tank":                                      {{}},
		"vgcreate stornas-tank /dev/md/stornas-tank":                         {{}},
		"lvs --noheadings --options lv_attr stornas-tank/thin":               {{err: errExit}},
		"lvcreate --type thin-pool --extents 90%VG --name thin stornas-tank": {{}},
		"readlink -f /dev/sda":                                               {{out: "/dev/sda\n"}},
		"readlink -f /dev/sdb":                                               {{out: "/dev/sdb\n"}},
		mdVgsCmd:                                                             {{out: vgsOut}},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "raid1", "/dev/sda", "/dev/sdb"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Health != "Online" || len(rep.Devices) != 2 {
		t.Fatalf("rep = %+v", rep)
	}
	for _, d := range rep.Devices {
		if d.State != "InSync" {
			t.Fatalf("devices = %+v", rep.Devices)
		}
	}
}

func TestEnsureRaidPoolIdempotent(t *testing.T) {
	f := &seqRunner{seq: map[string][]result{
		mdDetailCmd:        {{out: mdHealthy}, {out: mdHealthy}},
		"vgs stornas-tank": {{}},
		"lvs --noheadings --options lv_attr stornas-tank/thin": {{out: "  twi-aotz--\n"}},
		"readlink -f /dev/sda":                                 {{out: "/dev/sda\n"}},
		"readlink -f /dev/sdb":                                 {{out: "/dev/sdb\n"}},
		mdVgsCmd:                                               {{out: vgsOut}},
	}}

	if _, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "raid1", "/dev/sda", "/dev/sdb")); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "--create") || strings.HasPrefix(c, "pvcreate") || strings.HasPrefix(c, "lvcreate") {
			t.Fatalf("existing pool must not be recreated: %s", c)
		}
	}
}

// A pulled disk leaves a removed slot; the report must name the spec
// device that lost it so the UI replace flow can address the victim.
func TestEnsureRaidPoolDegradedNamesVictim(t *testing.T) {
	f := &seqRunner{seq: map[string][]result{
		mdDetailCmd:        {{out: mdDegraded}, {out: mdDegraded}},
		"vgs stornas-tank": {{}},
		"lvs --noheadings --options lv_attr stornas-tank/thin": {{out: "  twi-aotz--\n"}},
		"readlink -f /dev/sda":                                 {{out: "/dev/sda\n"}},
		"readlink -f /dev/sdb":                                 {{out: "/dev/sdb\n"}},
		"test -b /dev/sdb":                                     {{err: errExit}},
		mdVgsCmd:                                               {{out: vgsOut}},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "raid1", "/dev/sda", "/dev/sdb"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Health != "Degraded" {
		t.Fatalf("health = %s", rep.Health)
	}
	states := map[string]string{}
	for _, d := range rep.Devices {
		states[d.Path] = d.State
	}
	if states["/dev/sda"] != "InSync" || states["/dev/sdb"] != "Missing" {
		t.Fatalf("devices = %+v", rep.Devices)
	}
}

// The replace flow with a dead member: the newcomer fills the freed slot
// via --add and reports Rebuilding with progress.
func TestEnsureRaidPoolAddsReplacement(t *testing.T) {
	f := &seqRunner{seq: map[string][]result{
		mdDetailCmd:        {{out: mdDegraded}, {out: mdDegraded}, {out: mdRebuilding}},
		"vgs stornas-tank": {{}},
		"lvs --noheadings --options lv_attr stornas-tank/thin": {{out: "  twi-aotz--\n"}},
		"readlink -f /dev/sda":                                 {{out: "/dev/sda\n"}},
		"readlink -f /dev/sdc":                                 {{out: "/dev/sdc\n"}},
		"test -b /dev/sdc":                                     {{}},
		"mdadm /dev/md/stornas-tank --add /dev/sdc":            {{}},
		mdVgsCmd: {{out: vgsOut}},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "raid1", "/dev/sda", "/dev/sdc"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rebuild == nil || *rep.Rebuild != 43 {
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

// Swapping a live member must go through --replace so redundancy holds
// while the newcomer rebuilds.
func TestEnsureRaidPoolLiveReplace(t *testing.T) {
	f := &seqRunner{seq: map[string][]result{
		mdDetailCmd:        {{out: mdHealthy}, {out: mdHealthy}, {out: mdRebuilding}},
		"vgs stornas-tank": {{}},
		"lvs --noheadings --options lv_attr stornas-tank/thin":          {{out: "  twi-aotz--\n"}},
		"readlink -f /dev/sda":                                          {{out: "/dev/sda\n"}},
		"readlink -f /dev/sdc":                                          {{out: "/dev/sdc\n"}},
		"test -b /dev/sdc":                                              {{}},
		"mdadm /dev/md/stornas-tank --add-spare /dev/sdc":               {{}},
		"mdadm /dev/md/stornas-tank --replace /dev/sdb --with /dev/sdc": {{}},
		mdVgsCmd: {{out: vgsOut}},
	}}

	if _, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "raid1", "/dev/sda", "/dev/sdc")); err != nil {
		t.Fatal(err)
	}
	replaced := false
	for _, c := range f.calls {
		if c == "mdadm /dev/md/stornas-tank --replace /dev/sdb --with /dev/sdc" {
			replaced = true
		}
		if c == "mdadm /dev/md/stornas-tank --add /dev/sdc" {
			t.Fatalf("live swap must use --replace, not --add: %v", f.calls)
		}
	}
	if !replaced {
		t.Fatal("live member was not replaced")
	}
}

// A faulty member is swept (failed plus detached) before anything joins,
// and the report shows the freed slot as Missing.
func TestEnsureRaidPoolSweepsFaulty(t *testing.T) {
	faulty := `/dev/md/stornas-tank:
        Raid Level : raid1
             State : clean, degraded

    Number   Major   Minor   RaidDevice State
       0     252       16        0      active sync   /dev/sda
       1     252       32        1      faulty   /dev/sdb
`
	f := &seqRunner{seq: map[string][]result{
		mdDetailCmd:        {{out: faulty}, {out: faulty}, {out: mdDegraded}},
		"vgs stornas-tank": {{}},
		"lvs --noheadings --options lv_attr stornas-tank/thin": {{out: "  twi-aotz--\n"}},
		"mdadm /dev/md/stornas-tank --remove failed":           {{}},
		"mdadm /dev/md/stornas-tank --remove detached":         {{}},
		"readlink -f /dev/sda":                                 {{out: "/dev/sda\n"}},
		"readlink -f /dev/sdb":                                 {{out: "/dev/sdb\n"}},
		"test -b /dev/sdb":                                     {{err: errExit}},
		mdVgsCmd:                                               {{out: vgsOut}},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "raid1", "/dev/sda", "/dev/sdb"))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, d := range rep.Devices {
		states[d.Path] = d.State
	}
	if states["/dev/sdb"] != "Missing" {
		t.Fatalf("devices = %+v", rep.Devices)
	}
}

func TestEnsurePoolFailsClosed(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"vgs stornas-tank":  {err: errExit},
		"pvs /dev/sda":      {err: errExit},
		"pvcreate /dev/sda": {err: errExit},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), mdOf(f), pool("tank", "none", "/dev/sda"))
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
