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

func (l *LVM) LVExists(ctx context.Context, vg, lv string) bool {
	_, err := l.run.Run(ctx, "lvs", vg+"/"+lv)
	return err == nil
}

// CreateThinPool leaves VG headroom: thin metadata grows, and a full VG
// blocks lvextend during recovery.
func (l *LVM) CreateThinPool(ctx context.Context, vg, lv, raid string) error {
	if raid == "" || raid == "none" {
		_, err := l.run.Run(ctx, "lvcreate", "--type", "thin-pool", "--extents", "90%VG", "--name", lv, vg)
		return err
	}
	// A raid thin pool needs explicit data and metadata LVs; lvcreate
	// cannot build both in one call.
	if _, err := l.run.Run(ctx, "lvcreate", "--yes", "--type", raid, "--extents", "85%VG", "--name", lv, vg); err != nil {
		return err
	}
	if _, err := l.run.Run(ctx, "lvcreate", "--yes", "--type", raid, "--extents", "2%VG", "--name", lv+"_meta", vg); err != nil {
		return err
	}
	_, err := l.run.Run(ctx, "lvconvert", "--yes", "--type", "thin-pool", "--poolmetadata", vg+"/"+lv+"_meta", vg+"/"+lv)
	return err
}

type VGInfo struct {
	SizeBytes int64
	FreeBytes int64
}

type PV struct {
	Name    string
	Missing bool
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
	out, err := l.run.Run(ctx, "pvs", "--reportformat", "json",
		"--options", "pv_name,pv_missing", "--select", "vg_name="+vg)
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
			pvs = append(pvs, PV{Name: row["pv_name"], Missing: row["pv_missing"] != ""})
		}
	}
	return pvs, nil
}
