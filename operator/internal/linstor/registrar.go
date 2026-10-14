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

// EnsurePool registers vg's thin pool as the LINSTOR pool name on node and
// returns nil only once LINSTOR can place volumes on it.
// Existing pools keep their props: satellites own them after
// registration and rewriting them behind LINSTOR's back invites drift.
func (r *Registrar) EnsurePool(ctx context.Context, node, name, vg string) error {
	// A pool created while its satellite is disconnected lands in the
	// catalog with no error and no capacity, and LINSTOR never fetches
	// the capacity later (boot-test run 38546789566).
	n, err := r.client.Nodes.Get(ctx, node)
	if err != nil {
		return fmt.Errorf("get node %s: %w", node, err)
	}
	if n.ConnectionStatus != "ONLINE" {
		return fmt.Errorf("satellite %s is %s, registration waits for it", node, n.ConnectionStatus)
	}
	sp, err := r.client.Nodes.GetStoragePool(ctx, node, name)
	if errors.Is(err, lapi.NotFoundError) {
		return r.createPool(ctx, node, name, vg)
	}
	if err != nil {
		return fmt.Errorf("get storage pool %s on %s: %w", name, node, err)
	}
	// Another StoragePool of the same tier owns the name on this node;
	// touching it would hand that pool's volumes to this VG.
	if got := sp.Props[vgProp]; got != vg {
		return fmt.Errorf("storage pool %s on %s is registered to VG %s", name, node, got)
	}
	if rc := firstError(sp.Reports); rc != nil {
		return r.recreatePool(ctx, node, name, vg, "reports "+rc.String())
	}
	if sp.TotalCapacity <= 0 {
		return r.recreatePool(ctx, node, name, vg, "has no capacity on an online satellite")
	}
	return nil
}

var vgProp = golinstor.NamespcStorageDriver + "/" + golinstor.KeyStorPoolVolumeGroup

// recreatePool drops a pool LINSTOR will never make usable on its own, so
// the next pass creates it again. LINSTOR refuses the drop while the pool
// backs resources, and then the reason stands for a human.
func (r *Registrar) recreatePool(ctx context.Context, node, name, vg, reason string) error {
	if err := r.DeletePool(ctx, node, name, vg); err != nil {
		return fmt.Errorf("storage pool %s on %s %s; recreate: %w", name, node, reason, err)
	}
	return fmt.Errorf("storage pool %s on %s %s; recreating", name, node, reason)
}

// createPool always returns an error so the caller re-checks health on the
// next pass instead of trusting a create the satellite may not have applied.
func (r *Registrar) createPool(ctx context.Context, node, name, vg string) error {
	sp := lapi.StoragePool{
		StoragePoolName: name,
		ProviderKind:    lapi.LVM_THIN,
		Props: map[string]string{
			vgProp: vg,
			golinstor.NamespcStorageDriver + "/" + golinstor.KeyStorPoolThinPool: storagev1alpha1.ThinLV,
		},
	}
	if err := r.client.Nodes.CreateStoragePool(ctx, node, sp); err != nil {
		return fmt.Errorf("create storage pool %s on %s: %w", name, node, err)
	}
	return fmt.Errorf("storage pool %s on %s created, waiting for the satellite", name, node)
}

func firstError(reports []lapi.ApiCallRc) *lapi.ApiCallRc {
	for i := range reports {
		if uint64(reports[i].RetCode)&golinstor.MaskError == golinstor.MaskError {
			return &reports[i]
		}
	}
	return nil
}

// DeletePool removes this VG's registration from the LINSTOR catalog.
// The VG itself is host state and stays: pool deletion is a human decision.
// LINSTOR refuses while resources still live on the pool, which is what
// makes the finalizer wait instead of orphaning volumes. A registration
// held by another VG belongs to another pool and is left alone.
func (r *Registrar) DeletePool(ctx context.Context, node, name, vg string) error {
	sp, err := r.client.Nodes.GetStoragePool(ctx, node, name)
	if errors.Is(err, lapi.NotFoundError) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get storage pool %s on %s: %w", name, node, err)
	}
	if sp.Props[vgProp] != vg {
		return nil
	}
	err = r.client.Nodes.DeleteStoragePool(ctx, node, name)
	if err != nil && !errors.Is(err, lapi.NotFoundError) {
		return fmt.Errorf("delete storage pool %s on %s: %w", name, node, err)
	}
	return nil
}
