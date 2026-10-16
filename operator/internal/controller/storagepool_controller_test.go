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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var _ = Describe("StoragePool Controller", func() {
	ctx := context.Background()

	newPool := func(name, raid string, devices ...string) *storagev1alpha1.StoragePool {
		return &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       storagev1alpha1.StoragePoolSpec{Node: "node-a", Devices: devices, Raid: raid, Tier: name},
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
		// A reconciled pool carries the finalizer; run one pass to strip it
		// so the object actually goes away.
		r := &StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
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
		registered := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionRegistered)
		Expect(registered.Status).To(Equal(metav1.ConditionUnknown))
		Expect(registered.Reason).To(Equal(storagev1alpha1.ReasonHostNotReady))
	})

	getBackend := func(name string) *rdrbdv1alpha1.Pool {
		p := &rdrbdv1alpha1.Pool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, p)).To(Succeed())
		return p
	}

	// setAvailable plays the r-drbd agent.
	setAvailable := func(name string, status metav1.ConditionStatus, msg string) {
		p := getBackend(name)
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: rdrbdv1alpha1.ConditionAvailable, Status: status, Reason: "Test", Message: msg,
		})
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}

	// Every snapshot in a pool past this would fail on its device-mapper name.
	It("refuses a name whose VG leaves snapshots no device-mapper room", func() {
		err := k8sClient.Create(ctx, newPool("a-b-c-d-e-f-g-h", "none", "/dev/sda"))
		Expect(err).To(MatchError(ContainSubstring("name too long")))
		ok := newPool("aaaaaaaaaaaaaaaaaa-b", "none", "/dev/sda")
		Expect(k8sClient.Create(ctx, ok)).To(Succeed())
		deletePool(ok.Name)
	})

	It("registers the VG as an r-drbd Pool once the host is ready", func() {
		registered := newPool("registered", "none", "/dev/sda")
		registered.Spec.Tier = ""
		Expect(k8sClient.Create(ctx, registered)).To(Succeed())
		defer deletePool("registered")
		markHostReady("registered")

		r := &StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		pool, err := reconcileOnce(r, "registered")
		Expect(err).NotTo(HaveOccurred())
		// The API server fills r-drbd's capacity defaults: no overcommit.
		over, reserve := int32(100), int32(5)
		Expect(getBackend("registered").Spec).To(Equal(rdrbdv1alpha1.PoolSpec{
			Node: "node-a", Tier: storagev1alpha1.DefaultTier, VG: "stornas-registered", ThinPool: storagev1alpha1.ThinLV,
			OvercommitPercent: &over, ReservePercent: &reserve,
		}))
		available := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(available.Status).To(Equal(metav1.ConditionFalse))
		Expect(available.Reason).To(Equal(storagev1alpha1.ReasonWaitingForPool))

		By("mirroring the Pool's Available condition")
		setAvailable("registered", metav1.ConditionTrue, "")
		pool, err = reconcileOnce(r, "registered")
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionRegistered)).To(BeTrue())
	})

	It("surfaces the agent's reason when the Pool is unavailable", func() {
		Expect(k8sClient.Create(ctx, newPool("broken", "none", "/dev/sda"))).To(Succeed())
		defer deletePool("broken")
		markHostReady("broken")

		r := &StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconcileOnce(r, "broken")
		Expect(err).NotTo(HaveOccurred())
		setAvailable("broken", metav1.ConditionFalse, "thin pool stornas-broken/thin not found")
		pool, err := reconcileOnce(r, "broken")
		Expect(err).NotTo(HaveOccurred())
		cond := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonWaitingForPool))
		Expect(cond.Message).To(ContainSubstring("not found"))
	})

	// r-drbd pools are per node objects with a tier field, so a tier is no
	// longer a per node name (docs/architecture.md G6 in r-drbd).
	It("registers several pools of one tier on one node", func() {
		for _, n := range []string{"dup-a", "dup-b"} {
			p := newPool(n, "none", "/dev/sd"+n)
			p.Spec.Tier = "dup"
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			defer deletePool(n)
			markHostReady(n)
		}
		r := &StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for _, n := range []string{"dup-a", "dup-b"} {
			_, err := reconcileOnce(r, n)
			Expect(err).NotTo(HaveOccurred())
			Expect(getBackend(n).Spec.Tier).To(Equal("dup"))
		}
	})

	It("refuses a hand-made Pool of the same name on another VG", func() {
		Expect(k8sClient.Create(ctx, &rdrbdv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{Name: "squatted"},
			Spec:       rdrbdv1alpha1.PoolSpec{Node: "node-a", Tier: "squatted", VG: "other", ThinPool: "thin"},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, newPool("squatted", "none", "/dev/sda"))).To(Succeed())
		defer deletePool("squatted")
		markHostReady("squatted")

		pool, err := reconcileOnce(&StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, "squatted")
		Expect(err).NotTo(HaveOccurred())
		cond := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring("other"))
		Expect(getBackend("squatted").Spec.VG).To(Equal("other"))
	})

	It("deletes the r-drbd Pool and waits for its release and the host wipe", func() {
		setNode(ctx, "node-a", true)
		Expect(k8sClient.Create(ctx, newPool("finalized", "none", "/dev/sda"))).To(Succeed())
		markHostReady("finalized")

		r := &StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconcileOnce(r, "finalized")
		Expect(err).NotTo(HaveOccurred())

		By("r-drbd holding the Pool while a volume uses it")
		backend := getBackend("finalized")
		backend.Finalizers = []string{"r-drbd.io/pool"}
		Expect(k8sClient.Update(ctx, backend)).To(Succeed())
		backend.Status.Volumes = []string{"pvc-1"}
		Expect(k8sClient.Status().Update(ctx, backend)).To(Succeed())

		pool := &storagev1alpha1.StoragePool{ObjectMeta: metav1.ObjectMeta{Name: "finalized"}}
		Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
		_, rerr := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "finalized"}})
		Expect(rerr).NotTo(HaveOccurred())
		Expect(getBackend("finalized").DeletionTimestamp.IsZero()).To(BeFalse())
		got := &storagev1alpha1.StoragePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "finalized"}, got)).To(Succeed())
		dereg := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionDeregistered)
		Expect(dereg.Status).To(Equal(metav1.ConditionFalse))
		Expect(dereg.Message).To(ContainSubstring("pvc-1"))

		By("clearing the agent's wipe only after r-drbd released the Pool")
		backend = getBackend("finalized")
		backend.Finalizers = nil
		Expect(k8sClient.Update(ctx, backend)).To(Succeed())
		res, rerr := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "finalized"}})
		Expect(rerr).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(volumeSettleInterval))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "finalized"}, got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, storagev1alpha1.ConditionDeregistered)).To(BeTrue())

		By("the CR held until the agent confirms the wipe")
		meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
			Type:   storagev1alpha1.ConditionTornDown,
			Status: metav1.ConditionTrue,
			Reason: storagev1alpha1.ReasonReady,
		})
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		_, rerr = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "finalized"}})
		Expect(rerr).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "finalized"}, &storagev1alpha1.StoragePool{})
		Expect(err).To(HaveOccurred())
	})

	It("refuses a thick pool without creating an r-drbd Pool", func() {
		thick := newPool("thick", "none", "/dev/sda")
		thin := false
		thick.Spec.Thin = &thin
		Expect(k8sClient.Create(ctx, thick)).To(Succeed())
		defer deletePool("thick")
		markHostReady("thick")

		pool, err := reconcileOnce(&StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}, "thick")
		Expect(err).NotTo(HaveOccurred())
		registered := meta.FindStatusCondition(pool.Status.Conditions, storagev1alpha1.ConditionRegistered)
		Expect(registered.Status).To(Equal(metav1.ConditionFalse))
		Expect(registered.Reason).To(Equal(storagev1alpha1.ReasonUnsupported))
		Expect(registered.Message).To(Equal("r-drbd pools are thin only"))
		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, storagev1alpha1.ConditionAvailable)).To(BeFalse())
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "thick"}, &rdrbdv1alpha1.Pool{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("emits a warning event when the pool degrades", func() {
		Expect(k8sClient.Create(ctx, newPool("hurting", "raid1", "/dev/sda", "/dev/sdb"))).To(Succeed())
		defer deletePool("hurting")
		markHostReady("hurting")

		pool := &storagev1alpha1.StoragePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "hurting"}, pool)).To(Succeed())
		pool.Status.Health = "Degraded"
		pool.Status.Devices = []storagev1alpha1.DeviceStatus{
			{Path: "/dev/sda", State: "InSync"},
			{Path: "/dev/sdb", State: "Missing"},
		}
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())

		rec := events.NewFakeRecorder(4)
		r := &StoragePoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: rec}
		_, err := reconcileOnce(r, "hurting")
		Expect(err).NotTo(HaveOccurred())

		var ev string
		Eventually(rec.Events).Should(Receive(&ev))
		Expect(ev).To(ContainSubstring("PoolDegraded"))
		Expect(ev).To(ContainSubstring("/dev/sdb Missing"))
	})
})
