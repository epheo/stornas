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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var _ = Describe("Claim binder", func() {
	ctx := context.Background()

	ensureClass := func(name, provisioner string, mode storagev1.VolumeBindingMode) {
		sc := &storagev1.StorageClass{
			ObjectMeta:        metav1.ObjectMeta{Name: name},
			Provisioner:       provisioner,
			VolumeBindingMode: &mode,
		}
		if err := k8sClient.Create(ctx, sc); err != nil {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, sc)).To(Succeed())
		}
	}

	newPool := func(name, node, free string, available bool) *storagev1alpha1.StoragePool {
		pool := &storagev1alpha1.StoragePool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       storagev1alpha1.StoragePoolSpec{Node: node, Devices: []string{"/dev/disk/by-id/" + name}},
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
		pool.Status.Health = "Online"
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())
		return pool
	}

	dropPools := func(pools ...*storagev1alpha1.StoragePool) {
		for _, p := range pools {
			Expect(k8sClient.Delete(ctx, p)).To(Succeed())
		}
	}

	// declared mirrors the API: volumes it creates carry the consumer
	// annotation; undeclared claims model kubectl users.
	newClaim := func(name, class string, declared bool) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &class,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		if declared {
			pvc.Annotations = map[string]string{
				storagev1alpha1.ConsumerAnnotation: storagev1alpha1.ConsumerHost,
			}
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		return pvc
	}

	bindOnce := func(b *ClaimBinder, name string) (reconcile.Result, *corev1.PersistentVolumeClaim) {
		res, err := b.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: name},
		})
		Expect(err).NotTo(HaveOccurred())
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, pvc)).To(Succeed())
		return res, pvc
	}

	binder := func() *ClaimBinder { return &ClaimBinder{Client: k8sClient} }

	It("holds a declared claim while no pool is eligible", func() {
		ensureClass("bind-wffc", linstorProvisioner, storagev1.VolumeBindingWaitForFirstConsumer)
		setNode(ctx, "bind-node-a", true)
		pool := newPool("bind-sick", "bind-node-a", "100Gi", false)
		defer dropPools(pool)
		newClaim("bind-hold", "bind-wffc", true)
		res, pvc := bindOnce(binder(), "bind-hold")
		Expect(res.RequeueAfter).To(Equal(volumeSettleInterval))
		Expect(pvc.Annotations).NotTo(HaveKey(selectedNodeAnnotation))
	})

	It("binds a declared claim to the freest available pool node", func() {
		ensureClass("bind-wffc", linstorProvisioner, storagev1.VolumeBindingWaitForFirstConsumer)
		setNode(ctx, "bind-node-a", true)
		setNode(ctx, "bind-node-b", true)
		pools := []*storagev1alpha1.StoragePool{
			newPool("bind-small", "bind-node-a", "10Gi", true),
			newPool("bind-big", "bind-node-b", "100Gi", true),
		}
		defer dropPools(pools...)
		newClaim("bind-declared", "bind-wffc", true)
		_, pvc := bindOnce(binder(), "bind-declared")
		Expect(pvc.Annotations[selectedNodeAnnotation]).To(Equal("bind-node-b"))
	})

	It("never places on an unready node", func() {
		ensureClass("bind-wffc", linstorProvisioner, storagev1.VolumeBindingWaitForFirstConsumer)
		setNode(ctx, "bind-node-a", true)
		setNode(ctx, "bind-node-b", false)
		pools := []*storagev1alpha1.StoragePool{
			newPool("bind-small", "bind-node-a", "10Gi", true),
			newPool("bind-big", "bind-node-b", "100Gi", true),
		}
		defer dropPools(pools...)
		newClaim("bind-avoid-down", "bind-wffc", true)
		_, pvc := bindOnce(binder(), "bind-avoid-down")
		Expect(pvc.Annotations[selectedNodeAnnotation]).To(Equal("bind-node-a"))
	})

	It("leaves undeclared, unreferenced claims to upstream WFFC", func() {
		ensureClass("bind-wffc", linstorProvisioner, storagev1.VolumeBindingWaitForFirstConsumer)
		setNode(ctx, "bind-node-a", true)
		pool := newPool("bind-bare-pool", "bind-node-a", "100Gi", true)
		defer dropPools(pool)
		newClaim("bind-bare", "bind-wffc", false)
		res, pvc := bindOnce(binder(), "bind-bare")
		// No requeue: a Share or Target arriving re-enqueues via its watch.
		Expect(res).To(Equal(reconcile.Result{}))
		Expect(pvc.Annotations).NotTo(HaveKey(selectedNodeAnnotation))
	})

	It("treats a referencing Share as the first consumer", func() {
		ensureClass("bind-wffc", linstorProvisioner, storagev1.VolumeBindingWaitForFirstConsumer)
		setNode(ctx, "bind-node-a", true)
		pool := newPool("bind-share-pool", "bind-node-a", "100Gi", true)
		defer dropPools(pool)
		newClaim("bind-shared", "bind-wffc", false)
		share := &storagev1alpha1.Share{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-share", Namespace: "default"},
			Spec: storagev1alpha1.ShareSpec{
				ClaimName: "bind-shared",
				NFS:       &storagev1alpha1.NFSExport{Clients: []string{"192.168.1.0/24(rw)"}},
			},
		}
		Expect(k8sClient.Create(ctx, share)).To(Succeed())
		_, pvc := bindOnce(binder(), "bind-shared")
		Expect(pvc.Annotations[selectedNodeAnnotation]).To(Equal("bind-node-a"))
	})

	It("treats a referencing Target LUN as the first consumer", func() {
		ensureClass("bind-wffc", linstorProvisioner, storagev1.VolumeBindingWaitForFirstConsumer)
		setNode(ctx, "bind-node-a", true)
		pool := newPool("bind-lun-pool", "bind-node-a", "100Gi", true)
		defer dropPools(pool)
		newClaim("bind-lun", "bind-wffc", false)
		target := &storagev1alpha1.Target{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-target", Namespace: "default"},
			Spec: storagev1alpha1.TargetSpec{
				LUNs: []storagev1alpha1.LUN{{ID: 0, ClaimName: "bind-lun"}},
			},
		}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		_, pvc := bindOnce(binder(), "bind-lun")
		Expect(pvc.Annotations[selectedNodeAnnotation]).To(Equal("bind-node-a"))
	})

	It("ignores classes that are not ours or not WFFC", func() {
		ensureClass("bind-foreign", "ebs.csi.aws.com", storagev1.VolumeBindingWaitForFirstConsumer)
		ensureClass("bind-immediate", linstorProvisioner, storagev1.VolumeBindingImmediate)
		setNode(ctx, "bind-node-a", true)
		pool := newPool("bind-foreign-pool", "bind-node-a", "100Gi", true)
		defer dropPools(pool)
		for _, name := range []string{"bind-on-foreign", "bind-on-immediate"} {
			class := "bind-foreign"
			if name == "bind-on-immediate" {
				class = "bind-immediate"
			}
			newClaim(name, class, true)
			res, pvc := bindOnce(binder(), name)
			Expect(res).To(Equal(reconcile.Result{}))
			Expect(pvc.Annotations).NotTo(HaveKey(selectedNodeAnnotation))
		}
	})

	It("pins a restore to the snapshot's node, not the freest", func() {
		ensureClass("bind-wffc", linstorProvisioner, storagev1.VolumeBindingWaitForFirstConsumer)
		setNode(ctx, "bind-node-a", true)
		setNode(ctx, "bind-node-b", true)
		pools := []*storagev1alpha1.StoragePool{
			newPool("bind-src", "bind-node-a", "10Gi", true),
			newPool("bind-roomy", "bind-node-b", "100Gi", true),
		}
		defer dropPools(pools...)

		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-src-pv"},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: linstorProvisioner, VolumeHandle: "bind-src-pv"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pv)).To(Succeed())
		src := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "bind-src-claim", Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeName:  "bind-src-pv",
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())

		snap := &unstructured.Unstructured{}
		snap.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot",
		})
		snap.SetName("bind-snap")
		snap.SetNamespace("default")
		Expect(unstructured.SetNestedField(snap.Object,
			"bind-src-claim", "spec", "source", "persistentVolumeClaimName")).To(Succeed())
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		group := "snapshot.storage.k8s.io"
		restore := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: "bind-restore", Namespace: "default",
				Annotations: map[string]string{
					storagev1alpha1.ConsumerAnnotation: storagev1alpha1.ConsumerHost,
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: ptr("bind-wffc"),
				DataSource: &corev1.TypedLocalObjectReference{
					APIGroup: &group, Kind: "VolumeSnapshot", Name: "bind-snap",
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())

		b := &ClaimBinder{Client: k8sClient,
			Linstor: &fakePlacer{node: "bind-node-a", device: "/dev/drbd1000"}}
		_, pvc := bindOnce(b, "bind-restore")
		Expect(pvc.Annotations[selectedNodeAnnotation]).To(Equal("bind-node-a"))
	})
})

func ptr[T any](v T) *T { return &v }
