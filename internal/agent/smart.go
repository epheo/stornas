package agent

import (
	"context"
	"encoding/json"
	"sync"
)

// SmartInfo is the per-disk triage set: verdict plus the two numbers an
// operator reads first. Full attribute tables stay on the host.
type SmartInfo struct {
	Verdict      string // Passed | Failed | Unknown
	TempCelsius  *int32
	PowerOnHours *int64
}

// SmartStore shares SMART results between the inventory publisher (the
// one writer) and the pool reconciler inside one agent process, keyed by
// every path a device is known under (kernel and by-id).
type SmartStore struct {
	mu     sync.Mutex
	byPath map[string]SmartInfo
}

func NewSmartStore() *SmartStore {
	return &SmartStore{byPath: map[string]SmartInfo{}}
}

func (s *SmartStore) Put(info SmartInfo, paths ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range paths {
		if p != "" {
			s.byPath[p] = info
		}
	}
}

func (s *SmartStore) Get(path string) (SmartInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.byPath[path]
	return info, ok
}

// smartctlReport is the --json subset shared by ATA and NVMe devices.
type smartctlReport struct {
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current int32 `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int64 `json:"hours"`
	} `json:"power_on_time"`
}

// CheckSmart queries one device. smartctl exits nonzero when the disk is
// failing, so the output is parsed regardless of the exit code; -n standby
// leaves sleeping drives asleep (a NAS must not spin disks up to ask how
// they feel), in which case the caller keeps its cached answer.
func CheckSmart(ctx context.Context, run Runner, dev string) SmartInfo {
	out, _ := run.Run(ctx, "smartctl", "--json=c", "-H", "-A", "-n", "standby", dev)
	var rep smartctlReport
	if err := json.Unmarshal(out, &rep); err != nil || rep.SmartStatus == nil {
		return SmartInfo{Verdict: "Unknown"}
	}
	info := SmartInfo{Verdict: "Failed"}
	if rep.SmartStatus.Passed {
		info.Verdict = "Passed"
	}
	if rep.Temperature != nil {
		info.TempCelsius = &rep.Temperature.Current
	}
	if rep.PowerOnTime != nil {
		info.PowerOnHours = &rep.PowerOnTime.Hours
	}
	return info
}
