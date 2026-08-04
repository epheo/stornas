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
			VIP: "192.168.1.50/24",
			LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "disk0"}},
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
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000":  {},
		"targetcli /iscsi create " + iqn:                                              {},
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0": {},
		"targetcli /iscsi/" + iqn + "/tpg1/acls create iqn.1994-05.com.redhat:client1":     {},
		"targetcli /iscsi/" + iqn + "/tpg1/acls/iqn.1994-05.com.redhat:client1 set auth userid=u1 password=p1": {},
		"ip -j route show default":                    {out: `[{"dev":"eth0"}]`},
		"ip addr replace 192.168.1.50/24 dev eth0":    {},
		"targetcli saveconfig":                        {},
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

func TestEnsureTargetToleratesExisting(t *testing.T) {
	iqn := "iqn.2026-08.io.stornas:vms"
	exists := result{err: errExitWith("This Target already exists in configFS")}
	f := &fakeRunner{results: map[string]result{
		"targetcli /backstores/block create name=stornas-vms-lun0 dev=/dev/drbd1000":  exists,
		"targetcli /iscsi create " + iqn:                                              exists,
		"targetcli /iscsi/" + iqn + "/tpg1/luns create /backstores/block/stornas-vms-lun0": exists,
		"targetcli /iscsi/" + iqn + "/tpg1/acls create iqn.1994-05.com.redhat:client1":     exists,
		"ip -j route show default":                 {out: `[{"dev":"eth0"}]`},
		"ip addr replace 192.168.1.50/24 dev eth0": {},
		"targetcli saveconfig":                     {},
	}}
	m := &LIOManager{Run: f}

	if err := m.EnsureTarget(context.Background(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTargetDeletesByPrefix(t *testing.T) {
	f := &fakeRunner{results: map[string]result{
		"targetcli /iscsi delete iqn.2026-08.io.stornas:vms": {},
		"targetcli ls /backstores/block 1":                   {out: "o- block\n  o- stornas-vms-lun0 [/dev/drbd1000]\n  o- stornas-other-lun0 [x]\n"},
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
