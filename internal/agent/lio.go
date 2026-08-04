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

// ensure runs one targetcli command, tolerating "already exists" so
// re-convergence is a no-op walk.
func (m *LIOManager) ensure(ctx context.Context, args ...string) error {
	out, err := m.Run.Run(ctx, "targetcli", args...)
	if err != nil && !strings.Contains(err.Error()+string(out), "exist") {
		return err
	}
	return nil
}

func (m *LIOManager) EnsureTarget(ctx context.Context, t *storagev1alpha1.Target, chap map[string]ChapCred) error {
	iqn := t.Status.IQN
	for _, lun := range t.Status.LUNs {
		bs := backstore(t.Name, lun.ID)
		if err := m.ensure(ctx, "/backstores/block", "create", "name="+bs, "dev="+lun.Device); err != nil {
			return err
		}
		if err := m.ensure(ctx, "/iscsi", "create", iqn); err != nil {
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
	if t.Spec.VIP != "" {
		if err := m.ensureVIP(ctx, t.Spec.VIP); err != nil {
			return err
		}
	}
	_, err := m.Run.Run(ctx, "targetcli", "saveconfig")
	return err
}

// RemoveTarget tears down the export; backstores are found by prefix from
// the live tree because the Target object is already gone.
func (m *LIOManager) RemoveTarget(ctx context.Context, name string) {
	_, _ = m.Run.Run(ctx, "targetcli", "/iscsi", "delete", iqnFor(name))
	out, err := m.Run.Run(ctx, "targetcli", "ls", "/backstores/block", "1")
	if err == nil {
		prefix := "stornas-" + name + "-lun"
		for _, line := range strings.Fields(string(out)) {
			if strings.HasPrefix(line, prefix) {
				_, _ = m.Run.Run(ctx, "targetcli", "/backstores/block", "delete", line)
			}
		}
	}
	_, _ = m.Run.Run(ctx, "targetcli", "saveconfig")
}

func iqnFor(name string) string {
	return "iqn.2026-08.io.stornas:" + name
}

// ensureVIP puts the VIP on the default-route interface. `ip addr replace`
// is idempotent; gratuitous ARP is deferred to the failover milestone.
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
	_, err = m.Run.Run(ctx, "ip", "addr", "replace", vip, "dev", routes[0].Dev)
	return err
}
