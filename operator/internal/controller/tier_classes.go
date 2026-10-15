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
	"context"
	"fmt"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// TierClasses keeps a local and a replicated StorageClass for every tier a
// StoragePool names. They are clones of the shipped default-tier classes
// with the tier parameter swapped, so class policy lives in one manifest.
// StorageClass parameters are immutable: an existing class is left as is.
type TierClasses struct {
	client.Client
}

// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;delete

func (r *TierClasses) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	var pools storagev1alpha1.StoragePoolList
	if err := r.List(ctx, &pools); err != nil {
		return ctrl.Result{}, err
	}
	tiers := map[string]bool{}
	for _, p := range pools.Items {
		if p.Spec.Tier != "" && p.Spec.Tier != storagev1alpha1.DefaultTier {
			tiers[p.Spec.Tier] = true
		}
	}
	for tier := range tiers {
		local, replicated := storagev1alpha1.TierClasses(tier)
		if err := r.ensureClone(ctx, storagev1alpha1.LocalClass, local, tier); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.ensureClone(ctx, storagev1alpha1.ReplicatedClass, replicated, tier); err != nil {
			return ctrl.Result{}, err
		}
	}
	// A tier's classes go with its last pool; bound volumes keep their PVs.
	var classes storagev1.StorageClassList
	if err := r.List(ctx, &classes, client.HasLabels{storagev1alpha1.TierClassLabel}); err != nil {
		return ctrl.Result{}, err
	}
	for i := range classes.Items {
		if !tiers[classes.Items[i].Labels[storagev1alpha1.TierClassLabel]] {
			if err := r.Delete(ctx, &classes.Items[i]); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
	}
	return ctrl.Result{}, nil
}

func (r *TierClasses) ensureClone(ctx context.Context, from, name, tier string) error {
	var base storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: from}, &base); err != nil {
		return fmt.Errorf("base class %s: %w", from, err)
	}
	params := map[string]string{}
	for k, v := range base.Parameters {
		params[k] = v
	}
	params[rdrbdv1alpha1.ParamTier] = tier
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{storagev1alpha1.TierClassLabel: tier},
		},
		Provisioner:          base.Provisioner,
		Parameters:           params,
		ReclaimPolicy:        base.ReclaimPolicy,
		MountOptions:         base.MountOptions,
		AllowVolumeExpansion: base.AllowVolumeExpansion,
		VolumeBindingMode:    base.VolumeBindingMode,
	}
	if err := r.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create class %s: %w", name, err)
	}
	return nil
}

// SetupWithManager funnels every pool and class event into one key: the
// reconcile always recomputes the whole tier set.
func (r *TierClasses) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "tiers"}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("tierclasses").
		Watches(&storagev1alpha1.StoragePool{}, all).
		Watches(&storagev1.StorageClass{}, all).
		Complete(r)
}
