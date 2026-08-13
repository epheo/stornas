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

	golinstor "github.com/LINBIT/golinstor"
	lapi "github.com/LINBIT/golinstor/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

type Registrar struct {
	client *lapi.Client
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

// EnsurePool registers vg's thin pool under the shared pool name on node.
// Existing pools are trusted as-is: satellites own their props after
// registration and rewriting them behind LINSTOR's back invites drift.
func (r *Registrar) EnsurePool(ctx context.Context, node, vg string) error {
	_, err := r.client.Nodes.GetStoragePool(ctx, node, storagev1alpha1.LinstorPool)
	if err == nil {
		return nil
	}
	if !errors.Is(err, lapi.NotFoundError) {
		return fmt.Errorf("get storage pool %s on %s: %w", storagev1alpha1.LinstorPool, node, err)
	}
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
