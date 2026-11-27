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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var _ = Describe("Install plan", func() {
	ctx := context.Background()
	disk := func(path, size string) storagev1alpha1.Disk {
		q := resource.MustParse(size)
		return storagev1alpha1.Disk{Path: path, Size: &q}
	}
	// inventory makes node's inventory with disks and the installer's plan,
	// and reconciles it once.
	inventory := func(node, layout string, disks []storagev1alpha1.Disk, plan ...storagev1alpha1.PlannedPool) {
		inv := &storagev1alpha1.NodeInventory{ObjectMeta: metav1.ObjectMeta{Name: node}}
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())
		inv.Status.Disks, inv.Status.InstallPlan, inv.Status.InstallPlanLayout = disks, plan, layout
		Expect(k8sClient.Status().Update(ctx, inv)).To(Succeed())
		r := &InstallPlan{Client: k8sClient}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: node}})
		Expect(err).NotTo(HaveOccurred())
	}
	poolsOf := func(node string) map[string]storagev1alpha1.StoragePool {
		var list storagev1alpha1.StoragePoolList
		Expect(k8sClient.List(ctx, &list)).To(Succeed())
		out := map[string]storagev1alpha1.StoragePool{}
		for _, p := range list.Items {
			if p.Spec.Node == node {
				out[p.Name] = p
			}
		}
		return out
	}
	pair := func(name, tier string, devices ...string) storagev1alpha1.PlannedPool {
		return storagev1alpha1.PlannedPool{Name: name, Raid: "raid1", Tier: tier, Devices: devices}
	}
	// The cluster is shared with other specs: a pool left in tier ssd would
	// keep that tier's classes.
	AfterEach(func() {
		var pools storagev1alpha1.StoragePoolList
		Expect(k8sClient.List(ctx, &pools)).To(Succeed())
		for i := range pools.Items {
			if strings.HasPrefix(pools.Items[i].Spec.Node, "plan-") {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &pools.Items[i]))).To(Succeed())
			}
		}
		var invs storagev1alpha1.NodeInventoryList
		Expect(k8sClient.List(ctx, &invs)).To(Succeed())
		for i := range invs.Items {
			if strings.HasPrefix(invs.Items[i].Name, "plan-") {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &invs.Items[i]))).To(Succeed())
			}
		}
	})

	It("creates the planned pools once and leaves a deleted pool deleted", func() {
		taken := &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: "plan-taken"},
			Spec:       storagev1alpha1.StoragePoolSpec{Node: "plan-node", Devices: []string{"/dev/disk/by-id/wwn-c"}},
		}
		Expect(k8sClient.Create(ctx, taken)).To(Succeed())
		inventory("plan-node", "pairs",
			[]storagev1alpha1.Disk{disk("/dev/disk/by-id/wwn-a", "8Ti"), disk("/dev/disk/by-id/wwn-b", "8Ti"),
				disk("/dev/disk/by-id/wwn-c", "8Ti"), disk("/dev/disk/by-id/wwn-d", "8Ti")},
			pair("hdd-1", "hdd", "/dev/disk/by-id/wwn-a", "/dev/disk/by-id/wwn-b"),
			pair("hdd-2", "hdd", "/dev/disk/by-id/wwn-c", "/dev/disk/by-id/wwn-d"))

		// wwn-c already belongs to another pool: that planned pool is skipped.
		pools := poolsOf("plan-node")
		Expect(pools).To(HaveLen(2))
		var made storagev1alpha1.StoragePool
		for name, p := range pools {
			if name != "plan-taken" {
				made = p
			}
		}
		Expect(made.Spec.Raid).To(Equal("raid1"))
		Expect(made.Spec.Devices).To(ConsistOf("/dev/disk/by-id/wwn-a", "/dev/disk/by-id/wwn-b"))
		By("serving the default class: one kind of disk lands in tier default")
		Expect(made.Spec.Tier).To(Equal("default"))

		var inv storagev1alpha1.NodeInventory
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "plan-node"}, &inv)).To(Succeed())
		Expect(inv.Annotations).To(HaveKey(InstallPlanApplied))

		By("never recreating a pool the user deleted")
		Expect(k8sClient.Delete(ctx, &made)).To(Succeed())
		r := &InstallPlan{Client: k8sClient}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "plan-node"}})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			var p storagev1alpha1.StoragePool
			err := k8sClient.Get(ctx, types.NamespacedName{Name: made.Name}, &p)
			return apierrors.IsNotFound(err) || !p.DeletionTimestamp.IsZero()
		}).Should(BeTrue())
	})

	It("gives every machine its pools though each plan counts from 1", func() {
		for _, node := range []string{"plan-twin-1", "plan-twin-2"} {
			inventory(node, "pairs",
				[]storagev1alpha1.Disk{disk("/dev/disk/by-id/wwn-"+node+"-a", "4Ti"), disk("/dev/disk/by-id/wwn-"+node+"-b", "4Ti")},
				pair("hdd-1", "hdd", "/dev/disk/by-id/wwn-"+node+"-a", "/dev/disk/by-id/wwn-"+node+"-b"))
		}
		one, two := poolsOf("plan-twin-1"), poolsOf("plan-twin-2")
		Expect(one).To(HaveLen(1))
		Expect(two).To(HaveLen(1))
		for name := range one {
			Expect(two).NotTo(HaveKey(name))
		}
	})

	It("puts the kind holding the most capacity in tier default", func() {
		inventory("plan-mixed", "pairs",
			[]storagev1alpha1.Disk{disk("/dev/disk/by-id/wwn-m-s1", "1Ti"), disk("/dev/disk/by-id/wwn-m-s2", "1Ti"),
				disk("/dev/disk/by-id/wwn-m-h1", "8Ti"), disk("/dev/disk/by-id/wwn-m-h2", "8Ti")},
			pair("ssd-1", "ssd", "/dev/disk/by-id/wwn-m-s1", "/dev/disk/by-id/wwn-m-s2"),
			pair("hdd-1", "hdd", "/dev/disk/by-id/wwn-m-h1", "/dev/disk/by-id/wwn-m-h2"))
		tiers := map[string]string{}
		for _, p := range poolsOf("plan-mixed") {
			tiers[p.Spec.Devices[0]] = p.Spec.Tier
		}
		Expect(tiers).To(Equal(map[string]string{"/dev/disk/by-id/wwn-m-s1": "ssd", "/dev/disk/by-id/wwn-m-h1": "default"}))
	})

	It("leaves another image's plan, and disks the node does not offer", func() {
		inventory("plan-thin", "thin",
			[]storagev1alpha1.Disk{disk("/dev/disk/by-id/wwn-t-a", "4Ti")},
			storagev1alpha1.PlannedPool{Name: "data", Raid: "none", Devices: []string{"/dev/disk/by-id/wwn-t-a"}})
		Expect(poolsOf("plan-thin")).To(BeEmpty())

		inventory("plan-gone", "pairs",
			[]storagev1alpha1.Disk{disk("/dev/disk/by-id/wwn-g-a", "4Ti")},
			pair("hdd-1", "hdd", "/dev/disk/by-id/wwn-g-a", "/dev/disk/by-id/wwn-g-b"))
		Expect(poolsOf("plan-gone")).To(BeEmpty())
	})
})
