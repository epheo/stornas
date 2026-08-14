package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

const (
	shareRoot   = "/var/lib/stornas/shares"
	exportsDir  = "/etc/exports.d"
	sambaShares = "/etc/samba/stornas-shares.conf"
)

// ShareManager converges one node's mounts and exports from Share specs.
// Root prefixes every file write so tests run against a temp tree; commands
// always run through the Runner (nsenter on a real host).
type ShareManager struct {
	Run  Runner
	Node string
	Root string
}

func (m *ShareManager) mountPoint(ns, name string) string {
	return filepath.Join(shareRoot, ns+"-"+name)
}

func (m *ShareManager) exportsFile(ns, name string) string {
	return filepath.Join(exportsDir, "stornas-"+ns+"-"+name+".exports")
}

// EnsureShare mounts the placed device and converges the NFS export. The
// mount doubles as the DRBD promotion: auto-promote makes the first opener
// primary, which is why placement (status.node) must be decided upstream.
func (m *ShareManager) EnsureShare(ctx context.Context, share *storagev1alpha1.Share) error {
	mnt := m.mountPoint(share.Namespace, share.Name)
	src, err := m.Run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", mnt)
	mounted := err == nil
	if mounted && strings.TrimSpace(string(src)) != share.Status.Device {
		// A mount from a previous placement pins the wrong device; remount
		// so the export follows status.device.
		if _, err := m.Run.Run(ctx, "umount", mnt); err != nil {
			return err
		}
		mounted = false
	}
	if !mounted {
		// Fresh PVCs arrive raw: the CSI formats only when a pod mounts
		// the volume, so first placement owns the mkfs. Anything other
		// than blank or xfs is foreign data, never clobbered.
		fsout, _ := m.Run.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", share.Status.Device)
		switch fstype := strings.TrimSpace(string(fsout)); fstype {
		case "":
			if _, err := m.Run.Run(ctx, "mkfs.xfs", share.Status.Device); err != nil {
				return err
			}
		case "xfs":
		default:
			return fmt.Errorf("device %s carries %s, refusing to mount as xfs", share.Status.Device, fstype)
		}
		if err := os.MkdirAll(m.Root+mnt, 0o755); err != nil {
			return err
		}
		if _, err := m.Run.Run(ctx, "mount", "-t", "xfs", share.Status.Device, mnt); err != nil {
			return err
		}
		// A fresh xfs carries no labels and a pod-touched one carries
		// kubelet's container_file_t, which is customizable and only -F
		// resets; either way nfsd and smbd need the real context.
		// Best effort: enforcement may be off.
		if _, err := m.Run.Run(ctx, "restorecon", "-RF", mnt); err != nil {
			fmt.Printf("restorecon %s: %v\n", mnt, err)
		}
	}
	path := m.Root + m.exportsFile(share.Namespace, share.Name)
	if share.Spec.NFS != nil {
		line := mnt + " " + strings.Join(share.Spec.NFS.Clients, " ") + "\n"
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err = m.Run.Run(ctx, "exportfs", "-ra")
	return err
}

// Present reports whether this node still holds state for the share; the
// standby fast path, so teardown stays quiet where nothing was built.
func (m *ShareManager) Present(ns, name string) bool {
	if _, err := os.Stat(m.Root + m.mountPoint(ns, name)); err == nil {
		return true
	}
	_, err := os.Stat(m.Root + m.exportsFile(ns, name))
	return err == nil
}

// RemoveShare tears down what EnsureShare built; best effort because the
// share object may already be gone, but failures are logged so stale
// mounts and exports leave a trace. The unmount releases the DRBD device,
// which is what lets the new placement promote.
func (m *ShareManager) RemoveShare(ctx context.Context, ns, name string) {
	if err := os.Remove(m.Root + m.exportsFile(ns, name)); err != nil && !os.IsNotExist(err) {
		fmt.Printf("remove export %s-%s: %v\n", ns, name, err)
	}
	if _, err := m.Run.Run(ctx, "exportfs", "-ra"); err != nil {
		fmt.Printf("exportfs reload: %v\n", err)
	}
	if out, err := m.Run.Run(ctx, "umount", m.mountPoint(ns, name)); err != nil &&
		!strings.Contains(err.Error()+string(out), "not mounted") {
		fmt.Printf("umount %s-%s: %v\n", ns, name, err)
	}
	// The empty mountpoint is what Present keys on; keep teardown
	// convergent by removing it.
	_ = os.Remove(m.Root + m.mountPoint(ns, name))
}

// ApplySamba rewrites the single stornas include from every SMB share
// placed on this node and reloads. Whole-file regeneration makes share
// deletion fall out for free.
func (m *ShareManager) ApplySamba(ctx context.Context, shares []storagev1alpha1.Share) error {
	sort.Slice(shares, func(i, j int) bool {
		return shares[i].Namespace+"/"+shares[i].Name < shares[j].Namespace+"/"+shares[j].Name
	})
	var b strings.Builder
	for _, s := range shares {
		if s.Spec.SMB == nil || s.Status.Node != m.Node {
			continue
		}
		name := s.Spec.SMB.Name
		if name == "" {
			name = s.Name
		}
		// Shared-folder model, same as NFS's no_root_squash default:
		// valid users gate access, and inside the share everyone acts as
		// one owner. Without this no SMB user can write the root-owned
		// mountpoint; per-user ownership is post-v1 (needs ACL UX).
		fmt.Fprintf(&b, "[%s]\n\tpath = %s\n\tread only = no\n\tforce user = root\n",
			name, m.mountPoint(s.Namespace, s.Name))
		if len(s.Spec.SMB.ValidUsers) > 0 {
			fmt.Fprintf(&b, "\tvalid users = %s\n", strings.Join(s.Spec.SMB.ValidUsers, " "))
		}
		b.WriteString("\n")
	}
	if err := os.WriteFile(m.Root+sambaShares, []byte(b.String()), 0o644); err != nil {
		return err
	}
	_, err := m.Run.Run(ctx, "smbcontrol", "all", "reload-config")
	return err
}

// EnsureSMBUser provisions one user in the host samba passdb. The passdb
// maps to unix accounts, so an absent one is created first, nologin: SMB
// users never get a shell. smbpasswd -s is idempotent: re-applying the
// same password is a no-op in effect.
func EnsureSMBUser(ctx context.Context, run Runner, user, password string) error {
	if _, err := run.Run(ctx, "id", "-u", user); err != nil {
		if _, err := run.Run(ctx, "useradd", "--no-create-home", "--shell", "/sbin/nologin", user); err != nil {
			return err
		}
	}
	_, err := run.RunInput(ctx, password+"\n"+password+"\n", "smbpasswd", "-s", "-a", user)
	return err
}

// RemoveSMBUser revokes SMB access. The unix account stays: files keep
// their owner and a recreate reuses the uid. An absent passdb entry is
// the converged case (nodes that never provisioned the user).
func RemoveSMBUser(ctx context.Context, run Runner, user string) {
	out, err := run.Run(ctx, "smbpasswd", "-x", user)
	if err != nil && !strings.Contains(err.Error()+string(out), "find") {
		fmt.Printf("smbpasswd -x %s: %v\n", user, err)
	}
}
