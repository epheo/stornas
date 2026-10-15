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
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var _ = Describe("Tier classes", func() {
	ctx := context.Background()

	It("clones the shipped classes per tier and drops them with the tier's last pool", func() {
		mode := storagev1.VolumeBindingWaitForFirstConsumer
		for name, replicas := range map[string]string{
			storagev1alpha1.LocalClass:      "1",
			storagev1alpha1.ReplicatedClass: "2",
		} {
			base := &storagev1.StorageClass{
				ObjectMeta:        metav1.ObjectMeta{Name: name},
				Provisioner:       rdrbdv1alpha1.Driver,
				VolumeBindingMode: &mode,
				Parameters: map[string]string{
					rdrbdv1alpha1.ParamTier:     storagev1alpha1.DefaultTier,
					rdrbdv1alpha1.ParamReplicas: replicas,
				},
			}
			if err := k8sClient.Create(ctx, base); err != nil {
				Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
			}
		}
		pool := &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: "tier-ssd"},
			Spec:       storagev1alpha1.StoragePoolSpec{Node: "tier-node", Devices: []string{"/dev/sda"}, Tier: "ssd"},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		r := &TierClasses{Client: k8sClient}
		_, err := r.Reconcile(ctx, reconcile.Request{})
		Expect(err).NotTo(HaveOccurred())
		var sc storagev1.StorageClass
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "stornas-replicated-ssd"}, &sc)).To(Succeed())
		Expect(sc.Provisioner).To(Equal(rdrbdv1alpha1.Driver))
		Expect(sc.Parameters[rdrbdv1alpha1.ParamTier]).To(Equal("ssd"))
		Expect(sc.Parameters[rdrbdv1alpha1.ParamReplicas]).To(Equal("2"))
		Expect(*sc.VolumeBindingMode).To(Equal(mode))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "stornas-local-ssd"}, &sc)).To(Succeed())

		Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{})
		Expect(err).NotTo(HaveOccurred())
		for _, name := range []string{"stornas-local-ssd", "stornas-replicated-ssd"} {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &sc)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), name)
		}
	})
})
