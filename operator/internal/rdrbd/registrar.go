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

// Package rdrbd maps StoragePools onto r-drbd Pools and resolves where a
// Volume is served from its status.
package rdrbd

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// PoolSpec is the r-drbd Pool a StoragePool registers as. The Pool keeps
// the StoragePool's name, which is what the r-drbd.io/pool claim pin names.
func PoolSpec(sp *storagev1alpha1.StoragePool) rdrbdv1alpha1.PoolSpec {
	tier := sp.Spec.Tier
	if tier == "" {
		tier = storagev1alpha1.DefaultTier
	}
	return rdrbdv1alpha1.PoolSpec{
		Node:     sp.Spec.Node,
		Tier:     tier,
		VG:       sp.VGName(),
		ThinPool: storagev1alpha1.ThinLV,
	}
}

// EnsurePool creates sp's Pool when absent and reports ready once the
// r-drbd agent marks it Available. msg explains a false ready; err is
// reserved for API failures.
func EnsurePool(ctx context.Context, c client.Client, sp *storagev1alpha1.StoragePool) (ready bool, msg string, err error) {
	want := PoolSpec(sp)
	var pool rdrbdv1alpha1.Pool
	err = c.Get(ctx, types.NamespacedName{Name: sp.Name}, &pool)
	if apierrors.IsNotFound(err) {
		pool = rdrbdv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: sp.Name}, Spec: want}
		if err := c.Create(ctx, &pool); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, "", fmt.Errorf("create r-drbd pool %s: %w", sp.Name, err)
		}
		return false, "r-drbd pool created, waiting for the agent", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("get r-drbd pool %s: %w", sp.Name, err)
	}
	// Spec fields are immutable in r-drbd; a mismatch is a Pool someone
	// made by hand, and adopting it would hand its volumes to this VG.
	if pool.Spec != want {
		return false, fmt.Sprintf("r-drbd pool %s exists for %s/%s on %s, not %s/%s on %s",
			sp.Name, pool.Spec.VG, pool.Spec.ThinPool, pool.Spec.Node, want.VG, want.ThinPool, want.Node), nil
	}
	if !pool.DeletionTimestamp.IsZero() {
		return false, "r-drbd pool " + sp.Name + " is still being deleted", nil
	}
	cond := meta.FindStatusCondition(pool.Status.Conditions, rdrbdv1alpha1.ConditionAvailable)
	switch {
	case cond == nil:
		return false, "waiting for the r-drbd agent to report the pool", nil
	case cond.Status != metav1.ConditionTrue:
		return false, "r-drbd pool not available: " + cond.Message, nil
	}
	return true, "", nil
}

// DeletePool deletes sp's Pool and reports gone once r-drbd released it.
// r-drbd's finalizer holds the Pool while status.volumes is non-empty,
// which is what keeps the agent from wiping a VG that backs volumes.
func DeletePool(ctx context.Context, c client.Client, sp *storagev1alpha1.StoragePool) (gone bool, msg string, err error) {
	var pool rdrbdv1alpha1.Pool
	err = c.Get(ctx, types.NamespacedName{Name: sp.Name}, &pool)
	if apierrors.IsNotFound(err) {
		return true, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("get r-drbd pool %s: %w", sp.Name, err)
	}
	if pool.Spec.Node != sp.Spec.Node || pool.Spec.VG != sp.VGName() {
		return true, "", nil
	}
	if pool.DeletionTimestamp.IsZero() {
		if err := c.Delete(ctx, &pool); client.IgnoreNotFound(err) != nil {
			return false, "", fmt.Errorf("delete r-drbd pool %s: %w", sp.Name, err)
		}
	}
	if len(pool.Status.Volumes) > 0 {
		return false, "pool still backs volumes: " + strings.Join(pool.Status.Volumes, ", "), nil
	}
	return false, "waiting for r-drbd to release the pool", nil
}
