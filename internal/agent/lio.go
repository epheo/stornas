package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// vipRecordDir persists which VIP each target holds on this node: a CR
// deleted while the node is partitioned arrives later as a bare NotFound,
// and the record is then the only place the VIP is known.
const vipRecordDir = "/var/lib/stornas/vips"

// LIOManager converges the host LIO target config through targetcli. The
// host owns /etc/target/saveconfig.json (targetcli saveconfig), so exports
// survive reboots without the agent.
type LIOManager struct {
	Run Runner
	// Root prefixes host file paths so tests run against a temp tree.
	Root string
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
	auth := "0"
	for _, ini := range t.Spec.Initiators {
		if err := m.ensure(ctx, "/iscsi/"+iqn+"/tpg1/acls", "create", ini.IQN); err != nil {
			return err
		}
		if cred, ok := chap[ini.IQN]; ok {
			// targetcli takes the password only as an argument, so a
			// failure echoes it back through hostExec's error; scrub it
			// before it can reach Target status, events, or logs. The
			// transient argv exposure is accepted: the appliance grants
			// no interactive shells (SMB users are nologin), and root
			// reads the same secret from configfs anyway.
			if _, err := m.Run.Run(ctx, "targetcli", "/iscsi/"+iqn+"/tpg1/acls/"+ini.IQN,
				"set", "auth", "userid="+cred.User, "password="+cred.Password); err != nil {
				return errors.New(strings.ReplaceAll(err.Error(), cred.Password, "<redacted>"))
			}
		}
		if ini.ChapSecretRef != "" {
			auth = "1"
		}
	}
	// LIO enforces CHAP per TPG, not per ACL: at authentication=0 the
	// target only offers AuthMethod=None and CHAP initiators cannot log
	// in, so any CHAP initiator in the spec flips the whole TPG (keyed
	// on spec, not resolved secrets: a missing secret must fail closed,
	// never drop the target to no-auth). Mixed CHAP and no-CHAP
	// initiators on one target are not supportable.
	if _, err := m.Run.Run(ctx, "targetcli", "/iscsi/"+iqn+"/tpg1",
		"set", "attribute", "authentication="+auth); err != nil {
		return err
	}
	// The spec is authoritative: entries it dropped are revocations and
	// must leave the host, not linger until CR deletion.
	m.pruneACLs(ctx, t)
	m.pruneLUNs(ctx, t)
	if t.Spec.VIP != "" {
		if err := m.ensureVIP(ctx, t.Name, t.Spec.VIP); err != nil {
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
		m.dropVIP(ctx, vip)
	}
	m.releaseVIP(ctx, name)
}

// dropVIP removes vip from every interface holding it; absent is converged.
func (m *LIOManager) dropVIP(ctx context.Context, vip string) {
	for _, dev := range m.vipHolders(ctx, vip) {
		if _, err := m.Run.Run(ctx, "ip", "addr", "del", vip, "dev", dev); err != nil {
			fmt.Printf("ip addr del %s dev %s: %v\n", vip, dev, err)
		}
	}
}

func (m *LIOManager) vipRecord(name string) string {
	return filepath.Join(m.Root+vipRecordDir, name)
}

// releaseVIP drops the VIP recorded for name and the record itself; no
// record means nothing to release. This is the only teardown path that
// works after the spec is gone.
func (m *LIOManager) releaseVIP(ctx context.Context, name string) {
	rec := m.vipRecord(name)
	b, err := os.ReadFile(rec)
	if err != nil {
		return
	}
	m.dropVIP(ctx, strings.TrimSpace(string(b)))
	if err := os.Remove(rec); err != nil {
		fmt.Printf("remove vip record %s: %v\n", rec, err)
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
// the live tree and the VIP from the host record, because the Target
// object is already gone.
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
	m.releaseVIP(ctx, name)
}

func iqnFor(name string) string {
	return storagev1alpha1.IQNPrefix + name
}

// ensureVIP puts the VIP on the default-route interface. `ip addr replace`
// is idempotent; a fresh claim is announced with gratuitous ARP so
// switches and initiators repoint without waiting for cache expiry. GARP
// failure only logs: the export must not fail over a lost announce.
func (m *LIOManager) ensureVIP(ctx context.Context, name, vip string) error {
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
	// Recorded before the address lands: a crash between the two leaves a
	// record without an address, which releaseVIP treats as converged.
	if err := os.MkdirAll(m.Root+vipRecordDir, 0o755); err != nil {
		fmt.Printf("mkdir %s: %v\n", m.Root+vipRecordDir, err)
	} else if err := os.WriteFile(m.vipRecord(name), []byte(vip), 0o644); err != nil {
		fmt.Printf("record vip %s for %s: %v\n", vip, name, err)
	}
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
