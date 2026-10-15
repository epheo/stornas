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
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var _ = Describe("Claim binder", func() {
	ctx := context.Background()

	// Each spec uses its own tier: StoragePools outlive specs in envtest.
	ensureClass := func(name, tier, replicas string, mode storagev1.VolumeBindingMode) {
		sc := &storagev1.StorageClass{
			ObjectMeta:        metav1.ObjectMeta{Name: name},
			Provisioner:       rdrbdv1alpha1.Driver,
			VolumeBindingMode: &mode,
			Parameters:        map[string]string{rdrbdv1alpha1.ParamTier: tier, rdrbdv1alpha1.ParamReplicas: replicas},
		}
		Expect(k8sClient.Create(ctx, sc)).To(Succeed())
	}

	newPool := func(name, node, tier, free string, available bool) {
		pool := &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       storagev1alpha1.StoragePoolSpec{Node: node, Devices: []string{"/dev/disk/by-id/" + name}, Tier: tier},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		status := metav1.ConditionFalse
		if available {
			status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type: storagev1alpha1.ConditionAvailable, Status: status, Reason: "Test",
		})
		q := resource.MustParse(free)
		pool.Status.Free = &q
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())
	}

	newClaim := func(name, class string, annotations map[string]string, restore bool) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: annotations},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &class,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		if restore {
			group := "snapshot.storage.k8s.io"
			pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{APIGroup: &group, Kind: "VolumeSnapshot", Name: "snap"}
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	}

	host := func(extra ...string) map[string]string {
		a := map[string]string{storagev1alpha1.ConsumerAnnotation: storagev1alpha1.ConsumerHost}
		for i := 0; i+1 < len(extra); i += 2 {
			a[extra[i]] = extra[i+1]
		}
		return a
	}

	selected := func(name string) string {
		_, err := (&ClaimBinder{Client: k8sClient}).Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: name},
		})
		Expect(err).NotTo(HaveOccurred())
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, pvc)).To(Succeed())
		return pvc.Annotations[selectedNodeAnnotation]
	}

	wffc := storagev1.VolumeBindingWaitForFirstConsumer

	It("binds a pinned claim to its pool's node once the pool is Available", func() {
		setNode(ctx, "bind-a", true)
		setNode(ctx, "bind-b", true)
		ensureClass("bind-pin", "bindpin", "1", wffc)
		newPool("bind-pin-big", "bind-a", "bindpin", "500Gi", true)
		newPool("bind-pin-small", "bind-b", "bindpin", "10Gi", false)

		newClaim("pinned", "bind-pin", host(storagev1alpha1.PoolAnnotation, "bind-pin-small"), false)
		Expect(selected("pinned")).To(BeEmpty(), "pool not Available yet")

		p := &storagev1alpha1.StoragePool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "bind-pin-small"}, p)).To(Succeed())
		meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
			Type: storagev1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: "Test",
		})
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		Expect(selected("pinned")).To(Equal("bind-b"))

		By("honouring the pre r-drbd annotation too")
		newClaim("pinned-legacy", "bind-pin", host("storage.stornas.io/pool", "bind-pin-small"), false)
		Expect(selected("pinned-legacy")).To(Equal("bind-b"))
	})

	It("binds a local claim to the Ready node with the most free space in its tier", func() {
		setNode(ctx, "bind-c", true)
		setNode(ctx, "bind-d", true)
		setNode(ctx, "bind-e", false)
		defer setNode(ctx, "bind-e", true)
		ensureClass("bind-local", "bindlocal", "1", wffc)
		newPool("bind-local-c", "bind-c", "bindlocal", "50Gi", true)
		newPool("bind-local-d", "bind-d", "bindlocal", "80Gi", true)
		newPool("bind-local-e", "bind-e", "bindlocal", "900Gi", true)
		newPool("bind-local-c-sick", "bind-c", "bindlocal", "999Gi", false)
		newPool("bind-other-c", "bind-c", "bindother", "999Gi", true)

		newClaim("local", "bind-local", host(), false)
		Expect(selected("local")).To(Equal("bind-d"))

		By("leaving undeclared claims to the scheduler")
		newClaim("podclaim", "bind-local", nil, false)
		Expect(selected("podclaim")).To(BeEmpty())

		By("binding a restore too: r-drbd places it on the snapshot's node")
		newClaim("restore", "bind-local", host(), true)
		Expect(selected("restore")).NotTo(BeEmpty())
	})

	It("binds a replicated claim to a Ready node holding the tier", func() {
		setNode(ctx, "bind-f", true)
		setNode(ctx, "bind-g", true)
		setNode(ctx, "bind-h", true)
		ensureClass("bind-repl", "bindrepl", "2", wffc)
		newPool("bind-repl-f", "bind-f", "bindrepl", "10Gi", true)
		newPool("bind-repl-g", "bind-g", "bindrepl", "20Gi", true)

		newClaim("repl", "bind-repl", host(), false)
		Expect(selected("repl")).To(BeElementOf("bind-f", "bind-g"))

		By("ignoring Immediate classes")
		ensureClass("bind-imm", "bindrepl", "2", storagev1.VolumeBindingImmediate)
		newClaim("imm", "bind-imm", host(), false)
		Expect(selected("imm")).To(BeEmpty())
	})
})
