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

package linstor

import (
	"context"
	"fmt"
	"slices"
)

// ResolvePlacement picks the node that should serve a LINSTOR resource and
// the block device to open there. Priority: the current primary (moving an
// export off a live primary would break it), then prefer (sticky placement
// avoids flapping), then any diskful replica. Nodes in avoid never serve;
// skipping an avoided primary is the failover itself, since DRBD
// auto-promote lets the next opener take over once quorum fenced the old
// one. replicas counts diskful copies so callers can enforce
// replication-dependent spec rules.
func (r *Registrar) ResolvePlacement(ctx context.Context, resource, prefer string, avoid map[string]bool) (node, device string, replicas int, err error) {
	view, err := r.resourceView(ctx)
	if err != nil {
		return "", "", 0, fmt.Errorf("resource view: %w", err)
	}
	type replica struct {
		node, device string
		inUse        bool
	}
	var all []replica
	for _, res := range view {
		if res.Name != resource || len(res.Volumes) == 0 || res.Volumes[0].DevicePath == "" {
			continue
		}
		if slices.Contains(res.Flags, "DISKLESS") {
			continue
		}
		inUse := res.State != nil && res.State.InUse != nil && *res.State.InUse
		all = append(all, replica{res.NodeName, res.Volumes[0].DevicePath, inUse})
	}
	score := func(c replica) int {
		switch {
		case c.inUse:
			return 2
		case c.node == prefer:
			return 1
		default:
			return 0
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
		return "", "", len(all), fmt.Errorf("resource %s has no healthy diskful replica with a device", resource)
	}
	return all[pick].node, all[pick].device, len(all), nil
}
