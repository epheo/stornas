package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

func share(ns, name, node, device string, nfs *storagev1alpha1.NFSExport, smb *storagev1alpha1.SMBExport) storagev1alpha1.Share {
	return storagev1alpha1.Share{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       storagev1alpha1.ShareSpec{ClaimName: name, NFS: nfs, SMB: smb},
		Status:     storagev1alpha1.ShareStatus{Node: node, Device: device},
	}
}

func TestEnsureShareMountsAndExports(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/etc/exports.d", 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{results: map[string]result{
		"findmnt -n -o SOURCE /var/lib/stornas/shares/default-media":       {err: errExit},
		"blkid -o value -s TYPE /dev/drbd1000":                             {out: "xfs\n"},
		"mount -t xfs /dev/drbd1000 /var/lib/stornas/shares/default-media": {},
		"restorecon -R /var/lib/stornas/shares/default-media":              {},
		"exportfs -ra": {},
		"exportfs -f":  {},
	}}
	m := &ShareManager{Run: f, Node: "node-a", Root: root}
	s := share("default", "media", "node-a", "/dev/drbd1000",
		&storagev1alpha1.NFSExport{Clients: []string{"192.168.1.0/24(rw)"}}, nil)

	if err := m.EnsureShare(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(root + "/etc/exports.d/stornas-default-media.exports")
	if err != nil {
		t.Fatal(err)
	}
	want := "/var/lib/stornas/shares/default-media 192.168.1.0/24(rw)\n"
	if string(got) != want {
		t.Fatalf("exports = %q", got)
	}
}

func TestEnsureShareIdempotentMount(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/etc/exports.d", 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{results: map[string]result{
		"findmnt -n -o SOURCE /var/lib/stornas/shares/default-media": {out: "/dev/drbd1000\n"},
		"exportfs -ra": {},
		"exportfs -f":  {},
	}}
	m := &ShareManager{Run: f, Node: "node-a", Root: root}
	s := share("default", "media", "node-a", "/dev/drbd1000",
		&storagev1alpha1.NFSExport{Clients: []string{"10.0.0.0/8(ro)"}}, nil)

	if err := m.EnsureShare(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c[:5] == "mount" {
			t.Fatalf("mounted an already-mounted share: %s", c)
		}
	}
}

// A mount left by a previous placement points at the wrong device; the
// share must remount onto status.device, not trust the path alone.
func TestEnsureShareRemountsWrongDevice(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/etc/exports.d", 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{results: map[string]result{
		"findmnt -n -o SOURCE /var/lib/stornas/shares/default-media":       {out: "/dev/drbd1042\n"},
		"umount /var/lib/stornas/shares/default-media":                     {},
		"blkid -o value -s TYPE /dev/drbd1000":                             {out: "xfs\n"},
		"mount -t xfs /dev/drbd1000 /var/lib/stornas/shares/default-media": {},
		"restorecon -R /var/lib/stornas/shares/default-media":              {},
		"exportfs -ra": {},
		"exportfs -f":  {},
	}}
	m := &ShareManager{Run: f, Node: "node-a", Root: root}
	s := share("default", "media", "node-a", "/dev/drbd1000",
		&storagev1alpha1.NFSExport{Clients: []string{"10.0.0.0/8(ro)"}}, nil)

	if err := m.EnsureShare(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, c := range f.calls {
		if c[:6] == "umount" || c[:5] == "mount" {
			seen = append(seen, c)
		}
	}
	if len(seen) != 2 || seen[0][:6] != "umount" {
		t.Fatalf("calls = %v", f.calls)
	}
}

// A fresh PVC is a raw device: the CSI only formats volumes pods mount,
// so first placement must mkfs before mounting.
func TestEnsureShareFormatsRawDevice(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/etc/exports.d", 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{results: map[string]result{
		"findmnt -n -o SOURCE /var/lib/stornas/shares/default-media":       {err: errExit},
		"blkid -o value -s TYPE /dev/drbd1000":                             {err: errExit},
		"mkfs.xfs /dev/drbd1000":                                           {},
		"mount -t xfs /dev/drbd1000 /var/lib/stornas/shares/default-media": {},
		"restorecon -R /var/lib/stornas/shares/default-media":              {},
		"exportfs -ra": {},
		"exportfs -f":  {},
	}}
	m := &ShareManager{Run: f, Node: "node-a", Root: root}
	s := share("default", "media", "node-a", "/dev/drbd1000",
		&storagev1alpha1.NFSExport{Clients: []string{"*"}}, nil)

	if err := m.EnsureShare(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	formatted := false
	for _, c := range f.calls {
		if c == "mkfs.xfs /dev/drbd1000" {
			formatted = true
		}
	}
	if !formatted {
		t.Fatalf("raw device not formatted: %v", f.calls)
	}
}

// A foreign filesystem is someone's data; refusing beats clobbering it
// with mkfs or a forced xfs mount.
func TestEnsureShareRefusesForeignFilesystem(t *testing.T) {
	root := t.TempDir()
	f := &fakeRunner{results: map[string]result{
		"findmnt -n -o SOURCE /var/lib/stornas/shares/default-media": {err: errExit},
		"blkid -o value -s TYPE /dev/drbd1000":                       {out: "ext4\n"},
	}}
	m := &ShareManager{Run: f, Node: "node-a", Root: root}
	s := share("default", "media", "node-a", "/dev/drbd1000",
		&storagev1alpha1.NFSExport{Clients: []string{"*"}}, nil)

	if err := m.EnsureShare(context.Background(), &s); err == nil {
		t.Fatal("want error for a foreign filesystem")
	}
	for _, c := range f.calls {
		if c == "mkfs.xfs /dev/drbd1000" {
			t.Fatal("clobbered a foreign filesystem")
		}
	}
}

// Teardown must erase everything Present keys on, so a standby node that
// already converged never reruns the removal commands.
func TestRemoveShareConvergesPresence(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/etc/exports.d", 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{results: map[string]result{
		"findmnt -n -o SOURCE /var/lib/stornas/shares/default-media":       {err: errExit},
		"blkid -o value -s TYPE /dev/drbd1000":                             {out: "xfs\n"},
		"mount -t xfs /dev/drbd1000 /var/lib/stornas/shares/default-media": {},
		"restorecon -R /var/lib/stornas/shares/default-media":              {},
		"exportfs -ra": {},
		"exportfs -f":  {},
		"umount /var/lib/stornas/shares/default-media": {},
	}}
	m := &ShareManager{Run: f, Node: "node-a", Root: root}
	s := share("default", "media", "node-a", "/dev/drbd1000",
		&storagev1alpha1.NFSExport{Clients: []string{"*"}}, nil)
	if err := m.EnsureShare(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
	if !m.Present("default", "media") {
		t.Fatal("share not present after ensure")
	}

	m.RemoveShare(context.Background(), "default", "media")
	if m.Present("default", "media") {
		t.Fatal("share still present after removal")
	}
}

func TestApplySambaRendersOnlyLocalSMBShares(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/etc/samba", 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{results: map[string]result{
		"smbcontrol all reload-config": {},
	}}
	m := &ShareManager{Run: f, Node: "node-a", Root: root}
	// A deleting share lingers in the list until its finalizer releases;
	// rendering it would undo its teardown.
	now := metav1.Now()
	deleting := share("default", "leaving", "node-a", "/dev/drbd1003", nil,
		&storagev1alpha1.SMBExport{})
	deleting.DeletionTimestamp = &now
	shares := []storagev1alpha1.Share{
		share("default", "media", "node-a", "/dev/drbd1000", nil,
			&storagev1alpha1.SMBExport{ValidUsers: []string{"alice"}}),
		share("default", "elsewhere", "node-b", "/dev/drbd1001", nil,
			&storagev1alpha1.SMBExport{}),
		share("default", "nfsonly", "node-a", "/dev/drbd1002",
			&storagev1alpha1.NFSExport{Clients: []string{"*"}}, nil),
		deleting,
	}

	if err := m.ApplySamba(context.Background(), shares); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(root + "/etc/samba/stornas-shares.conf")
	if err != nil {
		t.Fatal(err)
	}
	want := "[media]\n\tpath = /var/lib/stornas/shares/default-media\n\tread only = no\n\tforce user = root\n\tvalid users = alice\n\n"
	if string(got) != want {
		t.Fatalf("samba conf = %q", got)
	}
}

func TestEnsureSMBUserFeedsPasswordTwice(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"id -u alice":           {out: "1001"},
		"smbpasswd -s -a alice": {},
	}}
	if err := EnsureSMBUser(context.Background(), f, "alice", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if f.stdins["smbpasswd -s -a alice"] != "hunter2\nhunter2\n" {
		t.Fatalf("stdin = %q", f.stdins["smbpasswd -s -a alice"])
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "useradd") {
			t.Fatalf("useradd ran for an existing account: %v", f.calls)
		}
	}
}

func TestEnsureSMBUserCreatesMissingUnixAccount(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"id -u bob": {err: errExit},
		"useradd --no-create-home --shell /sbin/nologin bob": {},
		"smbpasswd -s -a bob":                                {},
	}}
	if err := EnsureSMBUser(context.Background(), f, "bob", "hunter22"); err != nil {
		t.Fatal(err)
	}
	if f.stdins["smbpasswd -s -a bob"] != "hunter22\nhunter22\n" {
		t.Fatalf("stdin = %q", f.stdins["smbpasswd -s -a bob"])
	}
}

func TestRemoveSMBUserToleratesAbsentEntry(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"smbpasswd -x ghost": {out: "Failed to find entry for user ghost.", err: errExit},
	}}
	RemoveSMBUser(context.Background(), f, "ghost")
	if len(f.calls) != 1 {
		t.Fatalf("calls = %v", f.calls)
	}
}
