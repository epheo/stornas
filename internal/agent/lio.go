package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// LIOManager converges the host LIO target config through targetcli. The
// host owns /etc/target/saveconfig.json (targetcli saveconfig), so exports
// survive reboots without the agent.
type LIOManager struct {
	Run Runner
}

// ChapCred is one initiator's CHAP credential, resolved from its Secret by
// the reconciler; the manager never touches the API.
type ChapCred struct {
	User     string
	Password string
}

func backstore(target string, lunID int32) string {
	return fmt.Sprintf("stornas-%s-lun%d", target, lunID)
}

// ensure runs one targetcli create, tolerating the object-exists replies
// so re-convergence is a no-op walk. targetcli phrases them differently
// per object ("This Target already exists in configFS", "Storage object
// block/x exists"), while "does not exist" failures must surface, not
// converge green; the not-exist guard runs first because both phrasings
// contain "exist".
func (m *LIOManager) ensure(ctx context.Context, args ...string) error {
	out, err := m.Run.Run(ctx, "targetcli", args...)
	if err == nil {
		return nil
	}
	msg := err.Error() + string(out)
	if !strings.Contains(msg, "not exist") && strings.Contains(msg, "exists") {
		return nil
	}
	return err
}

// remove runs one targetcli delete; an absent entry is the converged case,
// anything else is logged because teardown has no status to report into.
func (m *LIOManager) remove(ctx context.Context, args ...string) {
	out, err := m.Run.Run(ctx, "targetcli", args...)
	if err != nil && !strings.Contains(err.Error()+string(out), "No such") {
		fmt.Printf("targetcli %s: %v\n", strings.Join(args, " "), err)
	}
}

func (m *LIOManager) EnsureTarget(ctx context.Context, t *storagev1alpha1.Target, chap map[string]ChapCred) error {
	iqn := t.Status.IQN
	if err := m.ensure(ctx, "/iscsi", "create", iqn); err != nil {
		return err
	}
	for _, lun := range t.Status.LUNs {
		bs := backstore(t.Name, lun.ID)
		if err := m.ensure(ctx, "/backstores/block", "create", "name="+bs, "dev="+lun.Device); err != nil {
			return err
		}
		if err := m.ensure(ctx, "/iscsi/"+iqn+"/tpg1/luns", "create", "/backstores/block/"+bs); err != nil {
			return err
		}
	}
	for _, ini := range t.Spec.Initiators {
		if err := m.ensure(ctx, "/iscsi/"+iqn+"/tpg1/acls", "create", ini.IQN); err != nil {
			return err
		}
		if cred, ok := chap[ini.IQN]; ok {
			if _, err := m.Run.Run(ctx, "targetcli", "/iscsi/"+iqn+"/tpg1/acls/"+ini.IQN,
				"set", "auth", "userid="+cred.User, "password="+cred.Password); err != nil {
				return err
			}
		}
	}
	// The spec is authoritative: entries it dropped are revocations and
	// must leave the host, not linger until CR deletion.
	m.pruneACLs(ctx, t)
	m.pruneLUNs(ctx, t)
	if t.Spec.VIP != "" {
		if err := m.ensureVIP(ctx, t.Spec.VIP); err != nil {
			return err
		}
	}
	_, err := m.Run.Run(ctx, "targetcli", "saveconfig")
	return err
}

func (m *LIOManager) pruneACLs(ctx context.Context, t *storagev1alpha1.Target) {
	want := map[string]bool{}
	for _, ini := range t.Spec.Initiators {
		want[ini.IQN] = true
	}
	out, err := m.Run.Run(ctx, "targetcli", "ls", "/iscsi/"+t.Status.IQN+"/tpg1/acls", "1")
	if err != nil {
		return
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, "iqn.") && !want[f] {
			m.remove(ctx, "/iscsi/"+t.Status.IQN+"/tpg1/acls", "delete", f)
		}
	}
}

func (m *LIOManager) pruneLUNs(ctx context.Context, t *storagev1alpha1.Target) {
	want := map[string]bool{}
	for _, lun := range t.Status.LUNs {
		want[backstore(t.Name, lun.ID)] = true
	}
	out, err := m.Run.Run(ctx, "targetcli", "ls", "/iscsi/"+t.Status.IQN+"/tpg1/luns", "1")
	if err != nil {
		return
	}
	// Only this target's backstores are candidates: the prefix guard keeps
	// hand-made or foreign entries out of reach.
	prefix := "stornas-" + t.Name + "-lun"
	for _, line := range strings.Split(string(out), "\n") {
		lun, bs := parseLUNLine(line)
		if lun == "" || !strings.HasPrefix(bs, prefix) || want[bs] {
			continue
		}
		m.remove(ctx, "/iscsi/"+t.Status.IQN+"/tpg1/luns", "delete", lun)
		m.remove(ctx, "/backstores/block", "delete", bs)
	}
}

// parseLUNLine picks the lun name and backstore out of one targetcli ls
// line, e.g. "o- lun0 ... [block/stornas-db-lun0 (/dev/drbd1000) ...]".
func parseLUNLine(line string) (lun, bs string) {
	for _, f := range strings.Fields(line) {
		if lun == "" && f != "luns" && strings.HasPrefix(f, "lun") {
			lun = f
		}
		if i := strings.Index(f, "block/"); i >= 0 {
			bs = strings.TrimPrefix(f[i:], "block/")
		}
	}
	return lun, bs
}

// TeardownTarget clears a target this node no longer serves, VIP included.
// The existence probes keep standby nodes quiet: absent means converged.
// Dropping the export and VIP on the losing node is the local half of
// failover fencing; DRBD quorum covers the partitioned case where this
// code cannot run.
func (m *LIOManager) TeardownTarget(ctx context.Context, name, vip string) {
	if _, err := m.Run.Run(ctx, "targetcli", "ls", "/iscsi/"+iqnFor(name), "1"); err == nil {
		m.RemoveTarget(ctx, name)
	}
	if vip != "" {
		for _, dev := range m.vipHolders(ctx, vip) {
			if _, err := m.Run.Run(ctx, "ip", "addr", "del", vip, "dev", dev); err != nil {
				fmt.Printf("ip addr del %s dev %s: %v\n", vip, dev, err)
			}
		}
	}
}

// vipHolders lists the interfaces currently holding vip; empty means the
// address is not on this node.
func (m *LIOManager) vipHolders(ctx context.Context, vip string) []string {
	addr := strings.SplitN(vip, "/", 2)[0]
	out, err := m.Run.Run(ctx, "ip", "-j", "addr", "show", "to", addr)
	if err != nil {
		return nil
	}
	var links []struct {
		Ifname string `json:"ifname"`
	}
	if err := json.Unmarshal(out, &links); err != nil {
		return nil
	}
	devs := make([]string, 0, len(links))
	for _, l := range links {
		devs = append(devs, l.Ifname)
	}
	return devs
}

// RemoveTarget tears down the export; backstores are found by prefix from
// the live tree because the Target object is already gone.
func (m *LIOManager) RemoveTarget(ctx context.Context, name string) {
	m.remove(ctx, "/iscsi", "delete", iqnFor(name))
	out, err := m.Run.Run(ctx, "targetcli", "ls", "/backstores/block", "1")
	if err == nil {
		prefix := "stornas-" + name + "-lun"
		for _, line := range strings.Fields(string(out)) {
			if strings.HasPrefix(line, prefix) {
				m.remove(ctx, "/backstores/block", "delete", line)
			}
		}
	}
	_, _ = m.Run.Run(ctx, "targetcli", "saveconfig")
}

func iqnFor(name string) string {
	return storagev1alpha1.IQNPrefix + name
}

// ensureVIP puts the VIP on the default-route interface. `ip addr replace`
// is idempotent; a fresh claim is announced with gratuitous ARP so
// switches and initiators repoint without waiting for cache expiry. GARP
// failure only logs: the export must not fail over a lost announce.
func (m *LIOManager) ensureVIP(ctx context.Context, vip string) error {
	out, err := m.Run.Run(ctx, "ip", "-j", "route", "show", "default")
	if err != nil {
		return err
	}
	var routes []struct {
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal(out, &routes); err != nil || len(routes) == 0 {
		return fmt.Errorf("no default route to place VIP %s on", vip)
	}
	dev := routes[0].Dev
	fresh := len(m.vipHolders(ctx, vip)) == 0
	if _, err := m.Run.Run(ctx, "ip", "addr", "replace", vip, "dev", dev); err != nil {
		return err
	}
	if fresh {
		addr := strings.SplitN(vip, "/", 2)[0]
		if _, err := m.Run.Run(ctx, "arping", "-U", "-c", "2", "-I", dev, addr); err != nil {
			fmt.Printf("garp %s on %s: %v\n", addr, dev, err)
		}
	}
	return nil
}
