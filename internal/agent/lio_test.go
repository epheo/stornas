package agent

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

func testTarget() *storagev1alpha1.Target {
	return &storagev1alpha1.Target{
		ObjectMeta: metav1.ObjectMeta{Name: "vms", Namespace: "stornas-system"},
		Spec: storagev1alpha1.TargetSpec{
			VIP:        "192.168.1.50/24",
			LUNs:       []storagev1alpha1.LUN{{ID: 0, ClaimName: "disk0"}},
			Initiators: []storagev1alpha1.Initiator{{IQN: "iqn.1994-05.com.redhat:client1", ChapSecretRef: "c1"}},
		},
		Status: storagev1alpha1.TargetStatus{
			IQN:        "iqn.2026-08.io.stornas:vms",
			ActiveNode: "node-a",
			LUNs:       []storagev1alpha1.LUNStatus{{ID: 0, Device: "/dev/drbd1000"}},
		},
	}
}

func TestEnsureTargetFullFlow(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	f := &fakeRunner{results: map[string]result{
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000":                           {},
		"targetcli /iscsi create " + iqn:                                                                       {},
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0":                     {},
		"targetcli /iscsi/" + iqn + "/tpg1/acls create iqn.1994-05.com.redhat:client1":                         {},
		"targetcli /iscsi/" + iqn + "/tpg1/acls/iqn.1994-05.com.redhat:client1 set auth userid=u1 password=p1": {},
		"targetcli /iscsi/" + iqn + "/tpg1 set attribute authentication=1":                                     {},

		"ip -j route show default":                 {out: `[{"dev":"eth0"}]`},
		"ip -j addr show to 192.168.1.50":          {out: `[]`},
		"ip addr replace 192.168.1.50/24 dev eth0": {},
		"arping -U -c 2 -I eth0 192.168.1.50":      {},
		"targetcli saveconfig":                     {},
	}}
	m := &LIOManager{Run: f}

	err := m.EnsureTarget(context.Background(), testTarget(), map[string]ChapCred{
		"iqn.1994-05.com.redhat:client1": {User: "u1", Password: "p1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls[len(f.calls)-1] != "targetcli saveconfig" {
		t.Fatalf("saveconfig must be last: %v", f.calls)
	}
}

// A VIP already on the node is a steady-state re-converge, not a claim;
// repeating the GARP every pass would spam the segment.
func TestEnsureVIPSkipsGarpWhenHeld(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	f := &fakeRunner{results: map[string]result{
		"targetcli /iscsi create " + iqn:                                                   {},
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000":       {},
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0": {},
		"targetcli /iscsi/" + iqn + "/tpg1/acls create iqn.1994-05.com.redhat:client1":     {},
		"targetcli /iscsi/" + iqn + "/tpg1 set attribute authentication=1":                 {},

		"ip -j route show default":                 {out: `[{"dev":"eth0"}]`},
		"ip -j addr show to 192.168.1.50":          {out: `[{"ifname":"eth0"}]`},
		"ip addr replace 192.168.1.50/24 dev eth0": {},
		"targetcli saveconfig":                     {},
	}}
	m := &LIOManager{Run: f}

	if err := m.EnsureTarget(context.Background(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "arping") {
			t.Fatalf("garp repeated on a held VIP: %v", f.calls)
		}
	}
}

func TestEnsureTargetToleratesExisting(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	exists := result{err: errExitWith("This Target already exists in configFS")}
	f := &fakeRunner{results: map[string]result{
		// The backstore phrasing has no "already"; both spellings must
		// converge.
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000": {
			err: errExitWith("Storage object block/stornas-vms-lun0 exists"),
		},
		"targetcli /iscsi create " + iqn: exists,
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0": exists,
		"targetcli /iscsi/" + iqn + "/tpg1/acls create iqn.1994-05.com.redhat:client1":     exists,
		"targetcli /iscsi/" + iqn + "/tpg1 set attribute authentication=1":                 {},

		"ip -j route show default":                 {out: `[{"dev":"eth0"}]`},
		"ip -j addr show to 192.168.1.50":          {out: `[{"ifname":"eth0"}]`},
		"ip addr replace 192.168.1.50/24 dev eth0": {},
		"targetcli saveconfig":                     {},
	}}
	m := &LIOManager{Run: f}

	if err := m.EnsureTarget(context.Background(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}
}

// A create failing because the backing device is gone must surface, not
// converge green on the "exist" substring.
func TestEnsureTargetSurfacesMissingDevice(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	f := &fakeRunner{results: map[string]result{
		"targetcli /iscsi create " + iqn: {},
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000": {
			err: errExitWith("/dev/drbd1000 does not exist"),
		},
	}}
	m := &LIOManager{Run: f}

	if err := m.EnsureTarget(context.Background(), testTarget(), nil); err == nil {
		t.Fatal("want error for a missing backing device")
	}
}

// Entries dropped from spec are revocations: the stale ACL and LUN must
// leave the host while the wanted ones stay.
func TestEnsureTargetPrunesDroppedEntries(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	aclsLs := "o- acls [ACLs: 2]\n" +
		"  o- iqn.1994-05.com.redhat:client1 [Mapped LUNs: 1]\n" +
		"  o- iqn.1994-05.com.redhat:client2 [Mapped LUNs: 1]\n"
	lunsLs := "o- luns [LUNs: 2]\n" +
		"  o- lun0 [block/stornas-vms-lun0 (/dev/drbd1000)]\n" +
		"  o- lun1 [block/stornas-vms-lun1 (/dev/drbd1001)]\n"
	f := &fakeRunner{results: map[string]result{
		"targetcli /iscsi create " + iqn:                                                   {},
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000":       {},
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0": {},
		"targetcli /iscsi/" + iqn + "/tpg1/acls create iqn.1994-05.com.redhat:client1":     {},
		"targetcli ls /iscsi/" + iqn + "/tpg1/acls 1":                                      {out: aclsLs},
		"targetcli ls /iscsi/" + iqn + "/tpg1/luns 1":                                      {out: lunsLs},
		"targetcli /iscsi/" + iqn + "/tpg1/acls delete iqn.1994-05.com.redhat:client2":     {},
		"targetcli /iscsi/" + iqn + "/tpg1/luns delete lun1":                               {},
		"targetcli /backstores/block delete stornas-vms-lun1":                              {},
		"targetcli /iscsi/" + iqn + "/tpg1 set attribute authentication=1":                 {},

		"ip -j route show default":                 {out: `[{"dev":"eth0"}]`},
		"ip -j addr show to 192.168.1.50":          {out: `[{"ifname":"eth0"}]`},
		"ip addr replace 192.168.1.50/24 dev eth0": {},
		"targetcli saveconfig":                     {},
	}}
	m := &LIOManager{Run: f}

	if err := m.EnsureTarget(context.Background(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}
	deleted := map[string]bool{}
	for _, c := range f.calls {
		if strings.Contains(c, "delete") {
			deleted[c] = true
		}
	}
	if !deleted["targetcli /iscsi/"+iqn+"/tpg1/acls delete iqn.1994-05.com.redhat:client2"] {
		t.Fatalf("stale ACL not revoked: %v", f.calls)
	}
	if !deleted["targetcli /iscsi/"+iqn+"/tpg1/luns delete lun1"] ||
		!deleted["targetcli /backstores/block delete stornas-vms-lun1"] {
		t.Fatalf("stale LUN not removed: %v", f.calls)
	}
	for c := range deleted {
		if strings.Contains(c, "client1") || strings.Contains(c, "lun0") {
			t.Fatalf("wanted entry deleted: %s", c)
		}
	}
}

// TPG auth follows spec intent: no CHAP initiators means authentication
// off; a CHAP initiator whose secret has not resolved yet must still
// lock the TPG, never fail open.
func TestEnsureTargetAuthFollowsSpec(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	open := testTarget()
	open.Spec.Initiators = []storagev1alpha1.Initiator{{IQN: "iqn.1994-05.com.redhat:client1"}}
	f := &fakeRunner{results: map[string]result{
		"targetcli /iscsi create " + iqn:                                                   {},
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000":       {},
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0": {},
		"targetcli /iscsi/" + iqn + "/tpg1/acls create iqn.1994-05.com.redhat:client1":     {},
		"targetcli /iscsi/" + iqn + "/tpg1 set attribute authentication=0":                 {},
		"ip -j route show default":                                                         {out: `[{"dev":"eth0"}]`},
		"ip -j addr show to 192.168.1.50":                                                  {out: `[{"ifname":"eth0"}]`},
		"ip addr replace 192.168.1.50/24 dev eth0":                                         {},
		"targetcli saveconfig":                                                             {},
	}}
	if err := (&LIOManager{Run: f}).EnsureTarget(context.Background(), open, nil); err != nil {
		t.Fatal(err)
	}

	// Same target with a chapSecretRef but no resolved cred: locked.
	locked := &fakeRunner{results: map[string]result{
		"targetcli /iscsi create " + iqn:                                                   {},
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000":       {},
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0": {},
		"targetcli /iscsi/" + iqn + "/tpg1/acls create iqn.1994-05.com.redhat:client1":     {},
		"targetcli /iscsi/" + iqn + "/tpg1 set attribute authentication=1":                 {},
		"ip -j route show default":                                                         {out: `[{"dev":"eth0"}]`},
		"ip -j addr show to 192.168.1.50":                                                  {out: `[{"ifname":"eth0"}]`},
		"ip addr replace 192.168.1.50/24 dev eth0":                                         {},
		"targetcli saveconfig":                                                             {},
	}}
	if err := (&LIOManager{Run: locked}).EnsureTarget(context.Background(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}
}

// The losing node must drop both the export and the VIP; keeping either
// would let a returned ex-primary serve stale data or answer ARP.
func TestTeardownTargetClearsExportAndVIP(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	f := &fakeRunner{results: map[string]result{
		"targetcli ls /iscsi/" + iqn + " 1":                   {out: "o- vms\n"},
		"targetcli /iscsi delete " + iqn:                      {},
		"targetcli ls /backstores/block 1":                    {out: "o- block\n  o- stornas-vms-lun0 [/dev/drbd1000]\n"},
		"targetcli /backstores/block delete stornas-vms-lun0": {},
		"targetcli saveconfig":                                {},
		"ip -j addr show to 192.168.1.50":                     {out: `[{"ifname":"eth0"}]`},
		"ip addr del 192.168.1.50/24 dev eth0":                {},
	}}
	m := &LIOManager{Run: f}
	m.TeardownTarget(context.Background(), "vms", "192.168.1.50/24")

	got := map[string]bool{}
	for _, c := range f.calls {
		got[c] = true
	}
	if !got["targetcli /iscsi delete "+iqn] || !got["ip addr del 192.168.1.50/24 dev eth0"] {
		t.Fatalf("teardown incomplete: %v", f.calls)
	}
}

// A standby that never held the target must only probe: running deletes
// and saveconfig on every reconcile would churn every idle node.
func TestTeardownTargetQuietWhenAbsent(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	f := &fakeRunner{results: map[string]result{
		"targetcli ls /iscsi/" + iqn + " 1": {err: errExitWith("No such path /iscsi/" + iqn)},
		"ip -j addr show to 192.168.1.50":   {out: `[]`},
	}}
	m := &LIOManager{Run: f}
	m.TeardownTarget(context.Background(), "vms", "192.168.1.50/24")

	if len(f.calls) != 2 {
		t.Fatalf("standby teardown not quiet: %v", f.calls)
	}
}

func TestRemoveTargetDeletesByPrefix(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"targetcli /iscsi delete iqn.2026-08.io.stornas:vms":  {},
		"targetcli ls /backstores/block 1":                    {out: "o- block\n  o- stornas-vms-lun0 [/dev/drbd1000]\n  o- stornas-other-lun0 [x]\n"},
		"targetcli /backstores/block delete stornas-vms-lun0": {},
		"targetcli saveconfig":                                {},
	}}
	m := &LIOManager{Run: f}
	m.RemoveTarget(context.Background(), "vms")

	for _, c := range f.calls {
		if strings.Contains(c, "delete stornas-other") {
			t.Fatalf("deleted another target's backstore: %v", f.calls)
		}
	}
}
