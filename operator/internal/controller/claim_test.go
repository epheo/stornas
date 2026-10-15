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

package controller

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/event"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
)

func TestVolumeServingChanged(t *testing.T) {
	base := func() *rdrbdv1alpha1.Volume {
		return &rdrbdv1alpha1.Volume{
			Spec: rdrbdv1alpha1.VolumeSpec{Assignment: &rdrbdv1alpha1.Assignment{Replicas: []rdrbdv1alpha1.AssignedReplica{
				{Node: "a", Role: rdrbdv1alpha1.RoleDiskful}, {Node: "b", Role: rdrbdv1alpha1.RoleDiskful},
			}}},
			Status: rdrbdv1alpha1.VolumeStatus{Primary: "a", Replicas: []rdrbdv1alpha1.ReplicaStatus{
				{Node: "a", Present: true, InUse: true, DevicePath: "/dev/drbd1000", DiskState: "UpToDate"},
				{Node: "b", Present: true, DevicePath: "/dev/drbd1000", DiskState: "Inconsistent"},
			}},
		}
	}
	pct := int32(40)
	cases := map[string]struct {
		mutate func(v *rdrbdv1alpha1.Volume)
		want   bool
	}{
		"sync progress":   {func(v *rdrbdv1alpha1.Volume) { v.Status.Replicas[1].SyncPercent = &pct }, false},
		"disk state":      {func(v *rdrbdv1alpha1.Volume) { v.Status.Replicas[1].DiskState = "UpToDate" }, true},
		"peer state":      {func(v *rdrbdv1alpha1.Volume) { v.Status.Replicas[0].Peers = map[string]string{"b": "Connecting"} }, false},
		"primary moves":   {func(v *rdrbdv1alpha1.Volume) { v.Status.Primary = "b" }, true},
		"device appears":  {func(v *rdrbdv1alpha1.Volume) { v.Status.Replicas[1].DevicePath = "" }, true},
		"replica absent":  {func(v *rdrbdv1alpha1.Volume) { v.Status.Replicas[1].Present = false }, true},
		"open moves":      {func(v *rdrbdv1alpha1.Volume) { v.Status.Replicas[0].InUse = false }, true},
		"role is dropped": {func(v *rdrbdv1alpha1.Volume) { v.Spec.Assignment.Replicas = v.Spec.Assignment.Replicas[:1] }, true},
	}
	for name, tc := range cases {
		newVol := base()
		tc.mutate(newVol)
		if got := volumeServingChanged.Update(event.UpdateEvent{ObjectOld: base(), ObjectNew: newVol}); got != tc.want {
			t.Errorf("%s: changed = %v, want %v", name, got, tc.want)
		}
	}
}
