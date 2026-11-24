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
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// InstallPlanApplied marks an inventory whose install plan was turned into
// pools; later edits to the host file never recreate a pool the user deleted.
const InstallPlanApplied = "storage.stornas.io/install-plan-applied"

// InstallPlan creates the pools chosen in the installer, once per node: the
// same objects the create-pool form makes, so pool lifecycle stays in one
// place.
type InstallPlan struct {
	client.Client
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.stornas.io,resources=nodeinventories,verbs=get;list;watch;patch

func (r *InstallPlan) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var inv storagev1alpha1.NodeInventory
	if err := r.Get(ctx, req.NamespacedName, &inv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if len(inv.Status.InstallPlan) == 0 || inv.Annotations[InstallPlanApplied] != "" {
		return ctrl.Result{}, nil
	}
	var pools storagev1alpha1.StoragePoolList
	if err := r.List(ctx, &pools); err != nil {
		return ctrl.Result{}, err
	}
	log := logf.FromContext(ctx)
	for _, planned := range inv.Status.InstallPlan {
		if reason := conflict(planned, inv.Name, pools.Items); reason != "" {
			log.Info("install plan pool skipped", "pool", planned.Name, "reason", reason)
			r.event(&inv, "PoolSkipped", planned.Name+": "+reason)
			continue
		}
		pool := &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: planned.Name},
			Spec: storagev1alpha1.StoragePoolSpec{
				Node:    inv.Name,
				Devices: planned.Devices,
				Raid:    planned.Raid,
				Tier:    planned.Tier,
			},
		}
		if err := r.Create(ctx, pool); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		r.event(&inv, "PoolCreated", planned.Name)
	}
	patch := client.MergeFrom(inv.DeepCopy())
	if inv.Annotations == nil {
		inv.Annotations = map[string]string{}
	}
	inv.Annotations[InstallPlanApplied] = "true"
	return ctrl.Result{}, r.Patch(ctx, &inv, patch)
}

// conflict says why a planned pool cannot be created as asked, or "".
func conflict(p storagev1alpha1.PlannedPool, node string, pools []storagev1alpha1.StoragePool) string {
	for _, existing := range pools {
		if existing.Name == p.Name {
			return "a pool with this name exists"
		}
		if existing.Spec.Node != node {
			continue
		}
		for _, d := range p.Devices {
			if slices.Contains(existing.Spec.Devices, d) {
				return d + " belongs to pool " + existing.Name
			}
		}
	}
	return ""
}

func (r *InstallPlan) event(inv *storagev1alpha1.NodeInventory, reason, note string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(inv, nil, corev1.EventTypeNormal, reason, "InstallPlan", "%s", note)
	}
}

func (r *InstallPlan) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.NodeInventory{}).
		Named("installplan").
		Complete(r)
}
