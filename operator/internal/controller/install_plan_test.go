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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var _ = Describe("Install plan", func() {
	ctx := context.Background()

	It("creates the planned pools once and leaves a deleted pool deleted", func() {
		taken := &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: "plan-taken"},
			Spec:       storagev1alpha1.StoragePoolSpec{Node: "plan-node", Devices: []string{"/dev/disk/by-id/wwn-c"}},
		}
		Expect(k8sClient.Create(ctx, taken)).To(Succeed())
		inv := &storagev1alpha1.NodeInventory{ObjectMeta: metav1.ObjectMeta{Name: "plan-node"}}
		Expect(k8sClient.Create(ctx, inv)).To(Succeed())
		inv.Status.InstallPlan = []storagev1alpha1.PlannedPool{
			{Name: "plan-hdd-1", Raid: "raid1", Tier: "hdd", Devices: []string{"/dev/disk/by-id/wwn-a", "/dev/disk/by-id/wwn-b"}},
			{Name: "plan-hdd-2", Raid: "raid1", Tier: "hdd", Devices: []string{"/dev/disk/by-id/wwn-c", "/dev/disk/by-id/wwn-d"}},
		}
		Expect(k8sClient.Status().Update(ctx, inv)).To(Succeed())

		r := &InstallPlan{Client: k8sClient}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "plan-node"}}
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var pool storagev1alpha1.StoragePool
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "plan-hdd-1"}, &pool)).To(Succeed())
		Expect(pool.Spec.Node).To(Equal("plan-node"))
		Expect(pool.Spec.Raid).To(Equal("raid1"))
		Expect(pool.Spec.Tier).To(Equal("hdd"))
		Expect(pool.Spec.Devices).To(HaveLen(2))
		// wwn-c already belongs to another pool: that planned pool is skipped.
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "plan-hdd-2"}, &pool)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		Expect(k8sClient.Get(ctx, req.NamespacedName, inv)).To(Succeed())
		Expect(inv.Annotations).To(HaveKey(InstallPlanApplied))

		By("never recreating a pool the user deleted")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "plan-hdd-1"}, &pool)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &pool)).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "plan-hdd-1"}, &pool)
			return apierrors.IsNotFound(err) || !pool.DeletionTimestamp.IsZero()
		}).Should(BeTrue())
	})
})
