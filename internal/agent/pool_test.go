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

func pool(name, raid string, devices ...string) *storagev1alpha1.StoragePool {
	return &storagev1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       storagev1alpha1.StoragePoolSpec{Node: "node-a", Devices: devices, Raid: raid},
	}
}

var errExit = fmt.Errorf("exit status 5")

func TestEnsurePoolFreshCreate(t *testing.T) {
	vgs := `{"report":[{"vg":[{"vg_size":"100","vg_free":"90"}]}]}`
	pvs := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":""}]}]}`
	f := &fakeRunner{results: map[string]result{
		"pvs /dev/sda":                   {err: errExit},
		"pvcreate /dev/sda":              {},
		"vgs stornas-tank":               {err: errExit},
		"vgcreate stornas-tank /dev/sda": {},
		"lvs stornas-tank/thin":          {err: errExit},
		"lvcreate --type thin-pool --extents 90%VG --name thin stornas-tank":                  {},
		"vgs --reportformat json --units b --nosuffix --options vg_size,vg_free stornas-tank": {out: vgs},
		"pvs --reportformat json --options pv_name,pv_missing --select vg_name=stornas-tank":  {out: pvs},
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
}

func TestEnsurePoolIdempotent(t *testing.T) {
	vgs := `{"report":[{"vg":[{"vg_size":"100","vg_free":"90"}]}]}`
	pvs := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":""},{"pv_name":"/dev/sdb","pv_missing":""}]}]}`
	f := &fakeRunner{results: map[string]result{
		"pvs /dev/sda":          {},
		"pvs /dev/sdb":          {},
		"vgs stornas-tank":      {},
		"lvs stornas-tank/thin": {},
		"vgs --reportformat json --units b --nosuffix --options vg_size,vg_free stornas-tank": {out: vgs},
		"pvs --reportformat json --options pv_name,pv_missing --select vg_name=stornas-tank":  {out: pvs},
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

func TestEnsurePoolDegradedOnMissingPV(t *testing.T) {
	vgs := `{"report":[{"vg":[{"vg_size":"100","vg_free":"90"}]}]}`
	pvs := `{"report":[{"pv":[{"pv_name":"/dev/sda","pv_missing":""},{"pv_name":"/dev/sdb","pv_missing":"missing"}]}]}`
	f := &fakeRunner{results: map[string]result{
		"pvs /dev/sda":          {},
		"pvs /dev/sdb":          {},
		"vgs stornas-tank":      {},
		"lvs stornas-tank/thin": {},
		"vgs --reportformat json --units b --nosuffix --options vg_size,vg_free stornas-tank": {out: vgs},
		"pvs --reportformat json --options pv_name,pv_missing --select vg_name=stornas-tank":  {out: pvs},
	}}

	rep, err := EnsurePool(context.Background(), lvm.NewWithRunner(f), pool("tank", "raid1", "/dev/sda", "/dev/sdb"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Health != "Degraded" {
		t.Fatalf("health = %s", rep.Health)
	}
}

func TestEnsurePoolFailsClosed(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
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
