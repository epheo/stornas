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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

type fakeRegistrar struct {
	calls []string
	err   error
}

func (f *fakeRegistrar) EnsurePool(_ context.Context, node, vg string) error {
	f.calls = append(f.calls, node+":"+vg)
	return f.err
}

var _ = Describe("StoragePool Controller", func() {
	ctx := context.Background()

	newPool := func(name, raid string, devices ...string) *storagev1alpha1.StoragePool {
		return &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       storagev1alpha1.StoragePoolSpec{Node: "node-a", Devices: devices, Raid: raid},
		}
	}

	reconcileOnce := func(r *StoragePoolReconciler, name string) (*storagev1alpha1.StoragePool, error) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		pool := &storagev1alpha1.StoragePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, pool)).To(Succeed())
		return pool, err
	}

	markHostReady := func(name string) {
		pool := &storagev1alpha1.StoragePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, pool)).To(Succeed())
		pool.Status.VG = pool.VGName()
		pool.Status.Health = "Online"
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:   storagev1alpha1.ConditionHostReady,
			Status: metav1.ConditionTrue,
			Reason: storagev1alpha1.ReasonReady,
		})
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())
	}

	deletePool := func(name string) {
		pool := &storagev1alpha1.StoragePool{ObjectMeta: metav1.ObjectMeta{Name: name}}
		Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
	}

	It("rejects raid levels the device count cannot carry", func() {
		Expect(k8sClient.Create(ctx, newPool("invalid", "raid5", "/dev/sda", "/dev/sdb"))).To(Succeed())
		defer deletePool("invalid")

		pool, err := reconcileOnce(&StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, "invalid")
		Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonInvalidSpec))
	})

	It("waits for the agent before going available", func() {
		Expect(k8sClient.Create(ctx, newPool("waiting", "none", "/dev/sda"))).To(Succeed())
		defer deletePool("waiting")

		pool, err := reconcileOnce(&StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, "waiting")
		Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonWaitingForAgent))
	})

	It("goes available without LINSTOR when no registrar is configured", func() {
		Expect(k8sClient.Create(ctx, newPool("hostonly", "none", "/dev/sda"))).To(Succeed())
		defer deletePool("hostonly")
		markHostReady("hostonly")

		pool, err := reconcileOnce(&StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, "hostonly")
		Expect(err).NotTo(HaveOccurred())

		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)).To(BeTrue())
		linstor := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionLinstorRegistered)
		Expect(linstor.Status).To(Equal(metav1.ConditionUnknown))
		Expect(linstor.Reason).To(Equal(storagev1alpha1.ReasonNotConfigured))
	})

	It("registers the VG with LINSTOR once the host is ready", func() {
		Expect(k8sClient.Create(ctx, newPool("registered", "none", "/dev/sda"))).To(Succeed())
		defer deletePool("registered")
		markHostReady("registered")

		reg := &fakeRegistrar{}
		pool, err := reconcileOnce(&StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: reg}, "registered")
		Expect(err).NotTo(HaveOccurred())

		Expect(reg.calls).To(Equal([]string{"node-a:stornas-registered"}))
		Expect(pool.Status.LinstorPool).To(Equal(storagev1alpha1.LinstorPool))
		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionLinstorRegistered)).To(BeTrue())
	})

	It("surfaces LINSTOR errors and retries", func() {
		Expect(k8sClient.Create(ctx, newPool("failing", "none", "/dev/sda"))).To(Succeed())
		defer deletePool("failing")
		markHostReady("failing")

		reg := &fakeRegistrar{err: fmt.Errorf("controller unreachable")}
		pool, err := reconcileOnce(&StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: reg}, "failing")
		Expect(err).To(HaveOccurred())

		cond := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonLinstorError))
	})
})
