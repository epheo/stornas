// Package lvm shells out to the host LVM tools. It is deliberately free of
// pool policy: names, raid levels, and sizes are the caller's decisions.
package lvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes one host command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type LVM struct {
	run Runner
}

func New() *LVM {
	return &LVM{run: execRunner{}}
}

func NewWithRunner(r Runner) *LVM {
	return &LVM{run: r}
}

func (l *LVM) IsPV(ctx context.Context, dev string) bool {
	_, err := l.run.Run(ctx, "pvs", dev)
	return err == nil
}

func (l *LVM) CreatePV(ctx context.Context, dev string) error {
	_, err := l.run.Run(ctx, "pvcreate", dev)
	return err
}

func (l *LVM) VGExists(ctx context.Context, vg string) bool {
	_, err := l.run.Run(ctx, "vgs", vg)
	return err == nil
}

func (l *LVM) CreateVG(ctx context.Context, vg string, devices []string) error {
	_, err := l.run.Run(ctx, "vgcreate", append([]string{vg}, devices...)...)
	return err
}

// IsThinPool distinguishes a finished pool from a bare LV left by an
// interrupted build; lv_attr starts with t only for thin pools.
func (l *LVM) IsThinPool(ctx context.Context, vg, lv string) bool {
	out, err := l.run.Run(ctx, "lvs", "--noheadings", "--options", "lv_attr", vg+"/"+lv)
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "t")
}

// CreateThinPool leaves VG headroom: thin metadata grows, and a full VG
// blocks lvextend during recovery. Always linear: raid lives in mdadm
// below the PV, never in LVM (README architecture).
func (l *LVM) CreateThinPool(ctx context.Context, vg, lv string) error {
	_, err := l.run.Run(ctx, "lvcreate", "--type", "thin-pool", "--extents", "90%VG", "--name", lv, vg)
	return err
}

// IsBlockDev reports whether the path is a live block device on the host.
func (l *LVM) IsBlockDev(ctx context.Context, dev string) bool {
	_, err := l.run.Run(ctx, "test", "-b", dev)
	return err == nil
}

// ResolvePath follows by-id symlinks to the kernel device so spec paths
// compare against pvs output. A path that cannot resolve (device gone)
// comes back unchanged.
func (l *LVM) ResolvePath(ctx context.Context, dev string) string {
	out, err := l.run.Run(ctx, "readlink", "-f", dev)
	if err != nil {
		return dev
	}
	if r := strings.TrimSpace(string(out)); r != "" {
		return r
	}
	return dev
}

func (l *LVM) VGExtend(ctx context.Context, vg, dev string) error {
	_, err := l.run.Run(ctx, "vgextend", vg, dev)
	return err
}

func (l *LVM) VGReduce(ctx context.Context, vg, dev string) error {
	_, err := l.run.Run(ctx, "vgreduce", vg, dev)
	return err
}

func (l *LVM) PVRemove(ctx context.Context, dev string) error {
	_, err := l.run.Run(ctx, "pvremove", dev)
	return err
}

// VGRemove wipes the VG and every LV on it; an absent VG is the
// converged case (pool teardown re-runs until confirmed).
func (l *LVM) VGRemove(ctx context.Context, vg string) error {
	out, err := l.run.Run(ctx, "vgremove", "-ff", "-y", vg)
	if err != nil && strings.Contains(string(out), "not found") {
		return nil
	}
	return err
}

// PVWipe clears the PV label after vgremove; "No PV label" means done,
// and a device gone from the host has nothing left to wipe.
func (l *LVM) PVWipe(ctx context.Context, dev string) error {
	out, err := l.run.Run(ctx, "pvremove", "-ff", "-y", dev)
	if err != nil && (strings.Contains(string(out), "o PV label") ||
		strings.Contains(string(out), "not found")) {
		return nil
	}
	return err
}

// PVVG reports whether dev carries a PV label and which VG claims it;
// "" with true means an orphan label (its VG is already gone).
func (l *LVM) PVVG(ctx context.Context, dev string) (string, bool) {
	out, err := l.run.Run(ctx, "pvs", "--noheadings", "--options", "vg_name", dev)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// PVMove starts a background evacuation of dev, onto dst when given,
// else wherever the allocator finds room; "already in progress" from a
// previous pass is not an error.
func (l *LVM) PVMove(ctx context.Context, dev, dst string) error {
	args := []string{"--background", dev}
	if dst != "" {
		args = append(args, dst)
	}
	out, err := l.run.Run(ctx, "pvmove", args...)
	if err != nil && strings.Contains(string(out), "in progress") {
		return nil
	}
	return err
}

type VGInfo struct {
	SizeBytes int64
	FreeBytes int64
}

type PV struct {
	Name    string
	Missing bool
	// UsedBytes is allocated extents; zero means safe to vgreduce.
	UsedBytes int64
}

// lvmReport matches the --reportformat json shape shared by vgs and pvs.
type lvmReport struct {
	Report []struct {
		VG []map[string]string `json:"vg"`
		PV []map[string]string `json:"pv"`
	} `json:"report"`
}

func (l *LVM) VGInfo(ctx context.Context, vg string) (VGInfo, error) {
	out, err := l.run.Run(ctx, "vgs", "--reportformat", "json", "--units", "b", "--nosuffix",
		"--options", "vg_size,vg_free", vg)
	if err != nil {
		return VGInfo{}, err
	}
	var rep lvmReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return VGInfo{}, fmt.Errorf("parse vgs report for %s: %w", vg, err)
	}
	var rows []map[string]string
	for _, r := range rep.Report {
		rows = append(rows, r.VG...)
	}
	if len(rows) == 0 {
		return VGInfo{}, fmt.Errorf("vg %s not in vgs report", vg)
	}
	size, err := strconv.ParseInt(rows[0]["vg_size"], 10, 64)
	if err != nil {
		return VGInfo{}, fmt.Errorf("parse vg_size %q: %w", rows[0]["vg_size"], err)
	}
	free, err := strconv.ParseInt(rows[0]["vg_free"], 10, 64)
	if err != nil {
		return VGInfo{}, fmt.Errorf("parse vg_free %q: %w", rows[0]["vg_free"], err)
	}
	return VGInfo{SizeBytes: size, FreeBytes: free}, nil
}

func (l *LVM) PVs(ctx context.Context, vg string) ([]PV, error) {
	out, err := l.run.Run(ctx, "pvs", "--reportformat", "json", "--units", "b", "--nosuffix",
		"--options", "pv_name,pv_missing,pv_used", "--select", "vg_name="+vg)
	if err != nil {
		return nil, err
	}
	var rep lvmReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("parse pvs report for %s: %w", vg, err)
	}
	var pvs []PV
	for _, r := range rep.Report {
		for _, row := range r.PV {
			used, _ := strconv.ParseInt(row["pv_used"], 10, 64)
			pvs = append(pvs, PV{Name: row["pv_name"], Missing: row["pv_missing"] != "", UsedBytes: used})
		}
	}
	return pvs, nil
}

// SyncPercent reports the lowest completion across raid resyncs and
// pvmove copies in the VG: nil when nothing is rebuilding. lvs shows
// sync_percent on raid LVs and copy_percent on active pvmove LVs.
func (l *LVM) SyncPercent(ctx context.Context, vg string) (*int32, error) {
	out, err := l.run.Run(ctx, "lvs", "--reportformat", "json", "-a",
		"--options", "lv_name,sync_percent,copy_percent", vg)
	if err != nil {
		return nil, err
	}
	var rep struct {
		Report []struct {
			LV []map[string]string `json:"lv"`
		} `json:"report"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("parse lvs report for %s: %w", vg, err)
	}
	var lowest *int32
	consider := func(s string) {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f >= 100 {
			return
		}
		p := int32(f)
		if lowest == nil || p < *lowest {
			lowest = &p
		}
	}
	for _, r := range rep.Report {
		for _, row := range r.LV {
			consider(row["sync_percent"])
			consider(row["copy_percent"])
		}
	}
	return lowest, nil
}
