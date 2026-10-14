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

// Package linstor registers stornas VGs as LINSTOR storage pools on the
// owning node's satellite.
package linstor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	golinstor "github.com/LINBIT/golinstor"
	lapi "github.com/LINBIT/golinstor/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

type Registrar struct {
	client *lapi.Client

	// One reconcile burst resolves many placements (a target's LUNs, the
	// allShares/allTargets sweeps) against what should be one cluster
	// view; the TTL collapses those into a single REST fetch. Staleness
	// is bounded well under volumeSettleInterval, so failover decisions
	// never act on an older picture than the next poll would.
	viewMu sync.Mutex
	viewAt time.Time
	view   []lapi.ResourceWithVolumes
}

const viewTTL = 5 * time.Second

func (r *Registrar) resourceView(ctx context.Context) ([]lapi.ResourceWithVolumes, error) {
	r.viewMu.Lock()
	defer r.viewMu.Unlock()
	if r.view != nil && time.Since(r.viewAt) < viewTTL {
		return r.view, nil
	}
	view, err := r.client.Resources.GetResourceView(ctx)
	if err != nil {
		return nil, err
	}
	r.view, r.viewAt = view, time.Now()
	return view, nil
}

func NewRegistrar(controllerURL string) (*Registrar, error) {
	u, err := url.Parse(controllerURL)
	if err != nil {
		return nil, fmt.Errorf("parse LINSTOR controller url %q: %w", controllerURL, err)
	}
	c, err := lapi.NewClient(lapi.BaseURL(u))
	if err != nil {
		return nil, err
	}
	return &Registrar{client: c}, nil
}

// EnsurePool registers vg's thin pool under the shared pool name on node
// and returns nil only once LINSTOR can place volumes on it.
// Existing pools keep their props: satellites own them after
// registration and rewriting them behind LINSTOR's back invites drift.
func (r *Registrar) EnsurePool(ctx context.Context, node, vg string) error {
	sp, err := r.client.Nodes.GetStoragePool(ctx, node, storagev1alpha1.LinstorPool)
	if errors.Is(err, lapi.NotFoundError) {
		return r.createPool(ctx, node, vg)
	}
	if err != nil {
		return fmt.Errorf("get storage pool %s on %s: %w", storagev1alpha1.LinstorPool, node, err)
	}
	if rc := firstError(sp.Reports); rc != nil {
		// A pool whose satellite apply failed stays in the catalog and
		// LINSTOR never retries it, so every volume on it would stall.
		// Recreating is safe: LINSTOR refuses to drop a pool backing
		// resources, and then the error stands for a human.
		if derr := r.DeletePool(ctx, node); derr != nil {
			return fmt.Errorf("storage pool %s on %s reports %s; recreate: %w",
				storagev1alpha1.LinstorPool, node, rc.String(), derr)
		}
		return fmt.Errorf("storage pool %s on %s reported %s; recreating",
			storagev1alpha1.LinstorPool, node, rc.String())
	}
	if sp.TotalCapacity <= 0 {
		return fmt.Errorf("storage pool %s on %s has no capacity reported yet",
			storagev1alpha1.LinstorPool, node)
	}
	return nil
}

// createPool always returns an error so the caller re-checks health on the
// next pass instead of trusting a create the satellite may not have applied.
func (r *Registrar) createPool(ctx context.Context, node, vg string) error {
	sp := lapi.StoragePool{
		StoragePoolName: storagev1alpha1.LinstorPool,
		ProviderKind:    lapi.LVM_THIN,
		Props: map[string]string{
			golinstor.NamespcStorageDriver + "/" + golinstor.KeyStorPoolVolumeGroup: vg,
			golinstor.NamespcStorageDriver + "/" + golinstor.KeyStorPoolThinPool:    storagev1alpha1.ThinLV,
		},
	}
	if err := r.client.Nodes.CreateStoragePool(ctx, node, sp); err != nil {
		return fmt.Errorf("create storage pool %s on %s: %w", storagev1alpha1.LinstorPool, node, err)
	}
	return fmt.Errorf("storage pool %s on %s created, waiting for the satellite",
		storagev1alpha1.LinstorPool, node)
}

func firstError(reports []lapi.ApiCallRc) *lapi.ApiCallRc {
	for i := range reports {
		if uint64(reports[i].RetCode)&golinstor.MaskError == golinstor.MaskError {
			return &reports[i]
		}
	}
	return nil
}

// DeletePool removes the node's registration from the LINSTOR catalog.
// The VG itself is host state and stays: pool deletion is a human decision.
// LINSTOR refuses while resources still live on the pool, which is what
// makes the finalizer wait instead of orphaning volumes.
func (r *Registrar) DeletePool(ctx context.Context, node string) error {
	err := r.client.Nodes.DeleteStoragePool(ctx, node, storagev1alpha1.LinstorPool)
	if err != nil && !errors.Is(err, lapi.NotFoundError) {
		return fmt.Errorf("delete storage pool %s on %s: %w", storagev1alpha1.LinstorPool, node, err)
	}
	return nil
}
