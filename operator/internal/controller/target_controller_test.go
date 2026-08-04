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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var _ = Describe("Target Controller", func() {
	ctx := context.Background()

	boundBlockClaim := func(name, volume string) {
		mode := corev1.PersistentVolumeBlock
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: volume},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeMode:  &mode,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "linstor.csi.linbit.com",
						VolumeHandle: volume,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pv)).To(Succeed())
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeName:  volume,
				VolumeMode:  &mode,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	}

	It("resolves devices, colocates LUNs, and waits for the agent", func() {
		boundBlockClaim("disk0", "pvc-disk0")
		target := &storagev1alpha1.Target{
			ObjectMeta: metav1.ObjectMeta{Name: "vms", Namespace: "default"},
			Spec: storagev1alpha1.TargetSpec{
				LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "disk0"}},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, target)).To(Succeed()) }()

		placer := &fakePlacer{node: "node-a", device: "/dev/drbd1001"}
		r := &TargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "vms"}})
		Expect(err).NotTo(HaveOccurred())

		got := &storagev1alpha1.Target{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "vms"}, got)).To(Succeed())
		Expect(got.Status.IQN).To(Equal("iqn.2026-08.io.stornas:vms"))
		Expect(got.Status.ActiveNode).To(Equal("node-a"))
		Expect(got.Status.LUNs).To(HaveLen(1))
		Expect(got.Status.LUNs[0].Device).To(Equal("/dev/drbd1001"))
		cond := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonWaitingForAgent))
	})

	It("waits for volumes while a claim is unbound", func() {
		target := &storagev1alpha1.Target{
			ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "default"},
			Spec: storagev1alpha1.TargetSpec{
				LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "nope"}},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, target)).To(Succeed()) }()

		r := &TargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: &fakePlacer{}}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pending"}})
		Expect(err).NotTo(HaveOccurred())

		got := &storagev1alpha1.Target{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "pending"}, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonWaitingForVolume))
	})
})
