/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rdrbd

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
)

// replica is one node of a test Volume: its assigned role and what its
// agent reports.
type replica struct {
	node  string
	role  rdrbdv1alpha1.Role
	inUse bool
}

func volume(primary string, replicas ...replica) *rdrbdv1alpha1.Volume {
	v := &rdrbdv1alpha1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec:       rdrbdv1alpha1.VolumeSpec{Assignment: &rdrbdv1alpha1.Assignment{}},
		Status:     rdrbdv1alpha1.VolumeStatus{Primary: primary},
	}
	for _, r := range replicas {
		v.Spec.Assignment.Replicas = append(v.Spec.Assignment.Replicas, rdrbdv1alpha1.AssignedReplica{Node: r.node, Role: r.role})
		v.Status.Replicas = append(v.Status.Replicas, rdrbdv1alpha1.ReplicaStatus{
			Node: r.node, Present: true, InUse: r.inUse, DevicePath: "/dev/drbd1000",
		})
	}
	return v
}

func TestResolvePlacementPrefersPrimary(t *testing.T) {
	v := volume("node-b",
		replica{node: "node-a", role: rdrbdv1alpha1.RoleDiskful},
		replica{node: "node-b", role: rdrbdv1alpha1.RoleDiskful},
		replica{node: "node-c", role: rdrbdv1alpha1.RoleTiebreaker})
	node, dev, replicas, err := ResolvePlacement(v, "node-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-b" || dev != "/dev/drbd1000" {
		t.Fatalf("placement = %s %s", node, dev)
	}
	if replicas != 2 {
		t.Fatalf("replicas = %d", replicas)
	}
}

// inUse alone also marks the serving node: replicas 1 volumes have no
// DRBD primary, only an open LV.
func TestResolvePlacementPrefersInUse(t *testing.T) {
	v := volume("",
		replica{node: "node-a", role: rdrbdv1alpha1.RoleDiskful},
		replica{node: "node-b", role: rdrbdv1alpha1.RoleDiskful, inUse: true})
	node, _, _, err := ResolvePlacement(v, "node-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-b" {
		t.Fatalf("node = %s", node)
	}
}

func TestResolvePlacementSkipsDisklessAndTiebreaker(t *testing.T) {
	v := volume("node-c",
		replica{node: "node-c", role: rdrbdv1alpha1.RoleDiskless, inUse: true},
		replica{node: "node-d", role: rdrbdv1alpha1.RoleTiebreaker},
		replica{node: "node-a", role: rdrbdv1alpha1.RoleDiskful})
	node, _, replicas, err := ResolvePlacement(v, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-a" {
		t.Fatalf("node = %s", node)
	}
	if replicas != 1 {
		t.Fatalf("replicas = %d", replicas)
	}
}

// A replica whose agent has not brought the device up cannot serve yet.
func TestResolvePlacementSkipsReplicaWithoutDevice(t *testing.T) {
	v := volume("", replica{node: "node-a", role: rdrbdv1alpha1.RoleDiskful})
	v.Status.Replicas[0].DevicePath = ""
	if _, _, _, err := ResolvePlacement(v, "", nil); err == nil {
		t.Fatal("want error")
	}
	v = volume("", replica{node: "node-a", role: rdrbdv1alpha1.RoleDiskful})
	v.Status.Replicas[0].Present = false
	if _, _, _, err := ResolvePlacement(v, "", nil); err == nil {
		t.Fatal("want error")
	}
}

func TestResolvePlacementNoReplica(t *testing.T) {
	if _, _, _, err := ResolvePlacement(volume(""), "", nil); err == nil {
		t.Fatal("want error")
	}
	if _, _, _, err := ResolvePlacement(&rdrbdv1alpha1.Volume{}, "", nil); err == nil {
		t.Fatal("want error without an assignment")
	}
}

// Failover: a primary on an avoided node must not win; the surviving
// diskful replica takes over.
func TestResolvePlacementAvoidsDeadPrimary(t *testing.T) {
	v := volume("node-a",
		replica{node: "node-a", role: rdrbdv1alpha1.RoleDiskful, inUse: true},
		replica{node: "node-b", role: rdrbdv1alpha1.RoleDiskful})
	node, _, replicas, err := ResolvePlacement(v, "node-a", map[string]bool{"node-a": true})
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-b" {
		t.Fatalf("node = %s", node)
	}
	if replicas != 2 {
		t.Fatalf("replicas = %d", replicas)
	}
}

// No primary and no preference signal: placement sticks to the current
// node instead of flapping to whichever replica lists first.
func TestResolvePlacementSticksToCurrent(t *testing.T) {
	v := volume("",
		replica{node: "node-a", role: rdrbdv1alpha1.RoleDiskful},
		replica{node: "node-b", role: rdrbdv1alpha1.RoleDiskful})
	node, _, _, err := ResolvePlacement(v, "node-b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-b" {
		t.Fatalf("node = %s", node)
	}
}

func TestResolvePlacementAllReplicasAvoided(t *testing.T) {
	v := volume("", replica{node: "node-a", role: rdrbdv1alpha1.RoleDiskful})
	if _, _, _, err := ResolvePlacement(v, "", map[string]bool{"node-a": true}); err == nil {
		t.Fatal("want error when every replica is unhealthy")
	}
}
