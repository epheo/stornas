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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	It("moves the target off an unready node and resets HostReady", func() {
		setNode(ctx, "node-a", true)
		setNode(ctx, "node-b", true)
		defer setNode(ctx, "node-a", true)

		boundBlockClaim("disk2", "pvc-disk2")
		target := &storagev1alpha1.Target{
			ObjectMeta: metav1.ObjectMeta{Name: "failover", Namespace: "default"},
			Spec: storagev1alpha1.TargetSpec{
				VIP:  "192.168.1.60/24",
				LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "disk2"}},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, target)).To(Succeed()) }()

		placer := &fakePlacer{node: "node-a", fallback: "node-b", device: "/dev/drbd1002", replicas: 2}
		r := &TargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}
		key := types.NamespacedName{Namespace: "default", Name: "failover"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		got := &storagev1alpha1.Target{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.ActiveNode).To(Equal("node-a"))

		By("the agent reporting the export up")
		meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
			Type:   storagev1alpha1.ConditionHostReady,
			Status: metav1.ConditionTrue,
			Reason: storagev1alpha1.ReasonReady,
		})
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		By("node-a going NotReady")
		setNode(ctx, "node-a", false)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.ActiveNode).To(Equal("node-b"))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, storagev1alpha1.ConditionHostReady)).To(BeFalse())
		cond := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonWaitingForAgent))
	})

	It("rejects a replicated LUN without a vip", func() {
		boundBlockClaim("disk3", "pvc-disk3")
		target := &storagev1alpha1.Target{
			ObjectMeta: metav1.ObjectMeta{Name: "novip", Namespace: "default"},
			Spec: storagev1alpha1.TargetSpec{
				LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "disk3"}},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, target)).To(Succeed()) }()

		placer := &fakePlacer{node: "node-a", device: "/dev/drbd1003", replicas: 2}
		r := &TargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "novip"}})
		Expect(err).NotTo(HaveOccurred())

		got := &storagev1alpha1.Target{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "novip"}, got)).To(Succeed())
		Expect(got.Status.ActiveNode).To(BeEmpty())
		cond := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonInvalidSpec))
	})

	It("surfaces placement errors and retries", func() {
		boundBlockClaim("disk1", "pvc-disk1")
		target := &storagev1alpha1.Target{
			ObjectMeta: metav1.ObjectMeta{Name: "erring-target", Namespace: "default"},
			Spec: storagev1alpha1.TargetSpec{
				LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "disk1"}},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, target)).To(Succeed()) }()

		placer := &fakePlacer{err: fmt.Errorf("controller unreachable")}
		r := &TargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "erring-target"}})
		// Flat requeue, no error: exponential backoff would strand
		// placement long after LINSTOR recovers.
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(volumeSettleInterval))

		got := &storagev1alpha1.Target{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "erring-target"}, got)).To(Succeed())
		cond := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonLinstorError))
	})

	It("holds deletion until the agent confirms teardown", func() {
		setNode(ctx, "node-a", true)
		boundBlockClaim("disk9", "pvc-disk9")
		target := &storagev1alpha1.Target{
			ObjectMeta: metav1.ObjectMeta{Name: "doomed", Namespace: "default"},
			Spec: storagev1alpha1.TargetSpec{
				VIP:  "192.168.1.61/24",
				LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "disk9"}},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())

		placer := &fakePlacer{node: "node-a", device: "/dev/drbd1009"}
		r := &TargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}
		key := types.NamespacedName{Namespace: "default", Name: "doomed"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		got := &storagev1alpha1.Target{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(teardownFinalizer))
		Expect(got.Status.ActiveNode).To(Equal("node-a"))

		By("deleting while the agent has not torn down yet")
		Expect(k8sClient.Delete(ctx, got)).To(Succeed())
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(volumeSettleInterval))
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())

		By("the agent confirming with State Removed")
		got.Status.State = "Removed"
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, got))).To(BeTrue())
	})

	It("releases deletion when the active node is unready", func() {
		setNode(ctx, "node-a", true)
		defer setNode(ctx, "node-a", true)
		boundBlockClaim("disk10", "pvc-disk10")
		target := &storagev1alpha1.Target{
			ObjectMeta: metav1.ObjectMeta{Name: "orphaned", Namespace: "default"},
			Spec: storagev1alpha1.TargetSpec{
				VIP:  "192.168.1.62/24",
				LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "disk10"}},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())

		placer := &fakePlacer{node: "node-a", device: "/dev/drbd1010"}
		r := &TargetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}
		key := types.NamespacedName{Namespace: "default", Name: "orphaned"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// A dead active node can never confirm; waiting would block
		// deletion forever.
		setNode(ctx, "node-a", false)
		got := &storagev1alpha1.Target{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(k8sClient.Delete(ctx, got)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, got))).To(BeTrue())
	})
})
