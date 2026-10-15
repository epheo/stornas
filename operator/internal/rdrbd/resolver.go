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
	"fmt"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
)

// ResolvePlacement picks the node that should serve a Volume and the
// block device to open there. An UpToDate replica always beats an
// Outdated or Inconsistent one: serving stale data is worse than moving.
// A primary whose disk is not UpToDate is therefore moved on purpose, a
// planned failover with a short export outage. Then: the current
// primary (moving an export off a live primary would break it), then
// prefer (sticky
// placement avoids flapping), then any diskful replica. Nodes in avoid
// never serve; skipping an avoided primary is the failover itself, since
// DRBD auto-promote lets the next opener take over once quorum fenced
// the old one. replicas counts diskful copies so callers can enforce
// replication-dependent spec rules.
func ResolvePlacement(v *rdrbdv1alpha1.Volume, prefer string, avoid map[string]bool) (node, device string, replicas int, err error) {
	type candidate struct {
		node, device string
		inUse        bool
		upToDate     bool
	}
	var all []candidate
	for _, r := range v.Status.Replicas {
		if !r.Present || r.DevicePath == "" || !v.Diskful(r.Node) {
			continue
		}
		all = append(all, candidate{r.Node, r.DevicePath, r.InUse || v.Status.Primary == r.Node, r.DiskState == "UpToDate"})
	}
	if v.Spec.Assignment != nil {
		for _, a := range v.Spec.Assignment.Replicas {
			if a.Role == rdrbdv1alpha1.RoleDiskful {
				replicas++
			}
		}
	}
	score := func(c candidate) int {
		s := 0
		if c.upToDate {
			s = 4
		}
		switch {
		case c.inUse:
			return s + 2
		case c.node == prefer:
			return s + 1
		default:
			return s
		}
	}
	pick := -1
	for i, c := range all {
		if avoid[c.node] {
			continue
		}
		if pick == -1 || score(c) > score(all[pick]) {
			pick = i
		}
	}
	if pick == -1 {
		return "", "", replicas, fmt.Errorf("volume %s has no healthy diskful replica with a device", v.Name)
	}
	return all[pick].node, all[pick].device, replicas, nil
}
