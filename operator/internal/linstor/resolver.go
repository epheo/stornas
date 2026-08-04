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
// the block device to mount there: the current primary when one exists
// (moving an export off a primary would break it), else the first diskful
// replica. DRBD auto-promote makes the mount itself perform the promotion.
func (r *Registrar) ResolvePlacement(ctx context.Context, resource string) (node, device string, err error) {
	view, err := r.client.Resources.GetResourceView(ctx)
	if err != nil {
		return "", "", fmt.Errorf("resource view: %w", err)
	}
	var diskful []int
	for i, res := range view {
		if res.Name != resource || len(res.Volumes) == 0 || res.Volumes[0].DevicePath == "" {
			continue
		}
		if slices.Contains(res.Flags, "DISKLESS") {
			continue
		}
		if res.State != nil && res.State.InUse != nil && *res.State.InUse {
			return res.NodeName, res.Volumes[0].DevicePath, nil
		}
		diskful = append(diskful, i)
	}
	if len(diskful) == 0 {
		return "", "", fmt.Errorf("resource %s has no diskful replica with a device", resource)
	}
	res := view[diskful[0]]
	return res.NodeName, res.Volumes[0].DevicePath, nil
}
