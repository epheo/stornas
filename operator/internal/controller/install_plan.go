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
	"maps"
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
// place. The plan is a proposal made per machine; names and tiers are
// decided here, for the cluster.
type InstallPlan struct {
	client.Client
	// Reader reads pools past the cache: a retry right after a create must
	// see that pool, or it would make a second one on the same disks.
	Reader   client.Reader
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
	// Another image's plan, such as the distro's thin pool on a node later
	// switched to stornas: its disks are in use, never stornas's to pair.
	if l := inv.Status.InstallPlanLayout; l != "" && l != "pairs" {
		return ctrl.Result{}, nil
	}
	var pools storagev1alpha1.StoragePoolList
	if err := r.reader().List(ctx, &pools); err != nil {
		return ctrl.Result{}, err
	}
	taken := map[string]bool{}
	for _, p := range pools.Items {
		taken[p.Name] = true
	}
	log := logf.FromContext(ctx)
	// made counts too when choosing tiers: a pass that failed halfway must
	// not change the tier of the pools a retry makes.
	var accepted, made []storagev1alpha1.PlannedPool
	gone := 0
	for _, planned := range inv.Status.InstallPlan {
		if madeBefore(planned, inv.Name, pools.Items) {
			made = append(made, planned)
			continue
		}
		reason := conflict(planned, inv.Name, pools.Items)
		if reason == "" {
			var absent bool
			reason, absent = missing(planned, inv.Status.Disks)
			if absent {
				gone++
			}
		}
		if reason != "" {
			log.Info("install plan pool skipped", "pool", planned.Name, "reason", reason)
			r.event(&inv, "PoolSkipped", planned.Name+": "+reason)
			continue
		}
		accepted = append(accepted, planned)
	}
	// Every disk absent is more likely a bad tick of the inventory (its
	// by-id names unread) than a plan for another machine: wait for the next.
	if gone == len(inv.Status.InstallPlan) {
		return ctrl.Result{}, nil
	}
	tiers := plannedTiers(append(slices.Clone(made), accepted...), inv.Status.Disks)
	for _, planned := range accepted {
		if err := r.create(ctx, &inv, planned, tiers[planned.Tier], taken); err != nil {
			return ctrl.Result{}, err
		}
	}
	patch := client.MergeFrom(inv.DeepCopy())
	if inv.Annotations == nil {
		inv.Annotations = map[string]string{}
	}
	inv.Annotations[InstallPlanApplied] = "true"
	return ctrl.Result{}, r.Patch(ctx, &inv, patch)
}

// create makes the planned pool under the first free name of its kind:
// every machine's plan counts from 1, and pool names are cluster-wide.
func (r *InstallPlan) create(ctx context.Context, inv *storagev1alpha1.NodeInventory,
	planned storagev1alpha1.PlannedPool, tier string, taken map[string]bool) error {
	kind := planned.Tier
	if kind == "" || len(kind) > 8 {
		kind = "pool"
	}
	for n := 1; n <= 999; n++ {
		name := fmt.Sprintf("%s-%d", kind, n)
		if taken[name] {
			continue
		}
		taken[name] = true
		pool := &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1alpha1.StoragePoolSpec{
				Node:    inv.Name,
				Devices: planned.Devices,
				Raid:    planned.Raid,
				Tier:    tier,
			},
		}
		err := r.Create(ctx, pool)
		if apierrors.IsAlreadyExists(err) {
			// This very pool, made by an earlier pass that failed later on;
			// or the name was taken since the list, by another node or a
			// person: then the next name.
			var existing storagev1alpha1.StoragePool
			if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &existing); err != nil {
				return err
			}
			if existing.Spec.Node == inv.Name && sameDevices(existing.Spec.Devices, planned.Devices) {
				return nil
			}
			continue
		}
		if apierrors.IsInvalid(err) {
			// A plan the schema refuses is never retried: a person can make
			// the pool by hand.
			r.event(inv, "PoolSkipped", planned.Name+": "+err.Error())
			return nil
		}
		if err != nil {
			return err
		}
		r.event(inv, "PoolCreated", name+", planned as "+planned.Name)
		return nil
	}
	return fmt.Errorf("no free pool name for %s", planned.Name)
}

// plannedTiers maps each planned disk kind to a tier. The default
// StorageClass and the UI's default placement serve tier default, so the
// node's pools land there; with two kinds, the one holding the most
// capacity does, and the other keeps its kind as its tier.
func plannedTiers(pools []storagev1alpha1.PlannedPool, disks []storagev1alpha1.Disk) map[string]string {
	if len(pools) == 0 {
		return nil
	}
	size := map[string]int64{}
	for _, d := range disks {
		if d.Size != nil {
			size[d.Path] = d.Size.Value()
		}
	}
	capacity := map[string]int64{}
	out := map[string]string{}
	for _, p := range pools {
		out[p.Tier] = p.Tier
		for _, d := range p.Devices {
			capacity[p.Tier] += size[d]
		}
	}
	kinds := slices.Sorted(maps.Keys(out))
	best := kinds[0]
	for _, k := range kinds[1:] {
		if capacity[k] > capacity[best] {
			best = k
		}
	}
	out[best] = "default"
	return out
}

// missing says which planned disk this node does not offer, or "", and
// whether it is absent rather than in use.
func missing(p storagev1alpha1.PlannedPool, disks []storagev1alpha1.Disk) (string, bool) {
	for _, d := range p.Devices {
		i := slices.IndexFunc(disks, func(x storagev1alpha1.Disk) bool { return x.Path == d })
		switch {
		case i < 0:
			return d + " is not on this node", true
		case disks[i].Claimed:
			return d + " is already in use", false
		}
	}
	return "", false
}

// madeBefore says whether this node already has the planned pool.
func madeBefore(p storagev1alpha1.PlannedPool, node string, pools []storagev1alpha1.StoragePool) bool {
	return slices.ContainsFunc(pools, func(x storagev1alpha1.StoragePool) bool {
		return x.Spec.Node == node && sameDevices(x.Spec.Devices, p.Devices)
	})
}

func sameDevices(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

func (r *InstallPlan) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// conflict says why a planned pool cannot be created as asked, or "".
func conflict(p storagev1alpha1.PlannedPool, node string, pools []storagev1alpha1.StoragePool) string {
	for _, existing := range pools {
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
