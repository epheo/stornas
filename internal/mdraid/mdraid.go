// Package mdraid drives md arrays through mdadm. Raid lives here, below
// the LVM PV (README architecture): the agent owns redundancy, LVM stays linear,
// and LINSTOR never sees a raid-backed thin pool.
package mdraid

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type MD struct {
	run Runner
}

func NewWithRunner(r Runner) *MD {
	return &MD{run: r}
}

// DevPath is the stable array node for a pool; mdadm's --name plus udev
// keep it constant across boots while /dev/mdN reshuffles.
func DevPath(pool string) string {
	return "/dev/md/stornas-" + pool
}

func (m *MD) Exists(ctx context.Context, dev string) bool {
	_, err := m.run.Run(ctx, "mdadm", "--detail", dev)
	return err == nil
}

// Create builds the array and returns immediately; the initial resync
// runs in the background and the array is usable throughout.
func (m *MD) Create(ctx context.Context, dev, name, level string, devices []string) error {
	args := []string{"--create", dev, "--run", "--force",
		"--name=" + name, "--homehost=stornas",
		"--level=" + level, "--raid-devices=" + strconv.Itoa(len(devices))}
	args = append(args, devices...)
	_, err := m.run.Run(ctx, "mdadm", args...)
	return err
}

// Member is one slot of the array; Path is empty for a removed slot
// whose disk is gone from the system. Failed means present but faulty,
// Missing means the slot lost its disk.
type Member struct {
	Path  string
	State string // InSync | Rebuilding | Failed | Missing
}

type Detail struct {
	Degraded bool
	// SyncPercent is nil when no rebuild or resync runs.
	SyncPercent *int32
	Members     []Member
}

func (m *MD) Detail(ctx context.Context, dev string) (Detail, error) {
	out, err := m.run.Run(ctx, "mdadm", "--detail", dev)
	if err != nil {
		return Detail{}, fmt.Errorf("mdadm detail %s: %w", dev, err)
	}
	return parseDetail(string(out)), nil
}

func parseDetail(out string) Detail {
	var d Detail
	inTable := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "State :"):
			d.Degraded = strings.Contains(t, "degraded")
		case strings.HasPrefix(t, "Rebuild Status :") || strings.HasPrefix(t, "Resync Status :"):
			if i := strings.Index(t, ":"); i >= 0 {
				pctStr, _, _ := strings.Cut(strings.TrimSpace(t[i+1:]), "%")
				if pct, err := strconv.ParseFloat(pctStr, 64); err == nil {
					p := int32(pct)
					d.SyncPercent = &p
				}
			}
		case strings.HasPrefix(t, "Number") && strings.Contains(t, "RaidDevice"):
			inTable = true
		case inTable && t != "":
			d.Members = append(d.Members, parseMember(t))
		}
	}
	return d
}

func parseMember(line string) Member {
	var m Member
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, "/dev/") {
			m.Path = f
		}
	}
	switch {
	case strings.Contains(line, "removed"):
		m.State = "Missing"
	case strings.Contains(line, "faulty"):
		m.State = "Failed"
	case strings.Contains(line, "rebuilding"), strings.Contains(line, "spare"):
		m.State = "Rebuilding"
	default:
		m.State = "InSync"
	}
	return m
}

// Replace rebuilds onto the newcomer before failing the old member, so
// redundancy never drops while swapping a live disk.
func (m *MD) Replace(ctx context.Context, dev, old, with string) error {
	if _, err := m.run.Run(ctx, "mdadm", dev, "--add-spare", with); err != nil {
		return err
	}
	_, err := m.run.Run(ctx, "mdadm", dev, "--replace", old, "--with", with)
	return err
}

func (m *MD) Add(ctx context.Context, dev, member string) error {
	_, err := m.run.Run(ctx, "mdadm", dev, "--add", member)
	return err
}

// FailDetached marks members whose device node vanished as faulty. An
// idle array does not notice a hot-unplugged disk until IO hits it, so
// detection would otherwise wait on a write.
func (m *MD) FailDetached(ctx context.Context, dev string) error {
	_, err := m.run.Run(ctx, "mdadm", dev, "--fail", "detached")
	return err
}

// RemoveFailed clears faulty and vanished members in one sweep; both
// keywords are mdadm-native and absent members are the converged case.
func (m *MD) RemoveFailed(ctx context.Context, dev string) error {
	if _, err := m.run.Run(ctx, "mdadm", dev, "--remove", "failed"); err != nil {
		return err
	}
	_, err := m.run.Run(ctx, "mdadm", dev, "--remove", "detached")
	return err
}

func (m *MD) Fail(ctx context.Context, dev, member string) error {
	_, err := m.run.Run(ctx, "mdadm", dev, "--fail", member)
	return err
}

// Stop disassembles the array on pool teardown; an already-absent array
// is the converged case.
func (m *MD) Stop(ctx context.Context, dev string) error {
	out, err := m.run.Run(ctx, "mdadm", "--stop", dev)
	if err != nil && (strings.Contains(string(out), "No such file") ||
		strings.Contains(err.Error(), "No such file")) {
		return nil
	}
	return err
}

// ZeroSuperblock erases the member signature so the disk reads unclaimed
// again; a member without one, or one whose disk vanished mid-teardown,
// is the converged case.
func (m *MD) ZeroSuperblock(ctx context.Context, member string) error {
	out, err := m.run.Run(ctx, "mdadm", "--zero-superblock", member)
	if err != nil && (strings.Contains(string(out), "Unrecognised") ||
		strings.Contains(string(out), "No such file") ||
		strings.Contains(string(out), "Couldn't open")) {
		return nil
	}
	return err
}

// ExamineName reads the array name from a member's superblock, "" when
// the device carries none. mdadm prints it as "host:name (local to
// host)"; only the name part identifies the array.
func (m *MD) ExamineName(ctx context.Context, member string) string {
	out, err := m.run.Run(ctx, "mdadm", "--examine", member)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "Name" {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(v))
		if len(fields) == 0 {
			return ""
		}
		name := fields[0]
		if i := strings.LastIndex(name, ":"); i >= 0 {
			name = name[i+1:]
		}
		return name
	}
	return ""
}

// Members lists kernel paths of current member disks straight from
// /proc/mdstat; inventory uses it to mark disks claimed.
func (m *MD) Members(ctx context.Context) map[string]bool {
	out, err := m.run.Run(ctx, "cat", "/proc/mdstat")
	if err != nil {
		return nil
	}
	members := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, " : ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			// member tokens look like "vdb[0]" or "vdc[1](F)"
			if i := strings.IndexByte(f, '['); i > 0 {
				members["/dev/"+f[:i]] = true
			}
		}
	}
	return members
}
