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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

type fakePlacer struct {
	node, device string
	fallback     string // wins when node is avoided, like a surviving replica
	replicas     int    // 0 means 1
	err          error
}

func (f *fakePlacer) ResolvePlacement(_ context.Context, _ string, _ string, avoid map[string]bool) (string, string, int, error) {
	node, replicas := f.node, f.replicas
	if avoid[node] && f.fallback != "" {
		node = f.fallback
	}
	if replicas == 0 {
		replicas = 1
	}
	return node, f.device, replicas, f.err
}

// setNode pins a node's Ready condition; envtest runs no node controller,
// so the value stays until the next call.
func setNode(ctx context.Context, name string, ready bool) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(ctx, node); err != nil {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
}

var _ = Describe("Share Controller", func() {
	ctx := context.Background()

	newShare := func(name, claim string) *storagev1alpha1.Share {
		return &storagev1alpha1.Share{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: storagev1alpha1.ShareSpec{
				ClaimName: claim,
				NFS:       &storagev1alpha1.NFSExport{Clients: []string{"192.168.1.0/24(rw)"}},
			},
		}
	}

	boundClaim := func(name, volume string) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: volume},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
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
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	}

	reconcileOnce := func(r *ShareReconciler, name string) (*storagev1alpha1.Share, error) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
		share := &storagev1alpha1.Share{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, share)).To(Succeed())
		return share, err
	}

	It("waits for the volume while the claim is missing", func() {
		Expect(k8sClient.Create(ctx, newShare("unbound", "no-such-claim"))).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, newShare("unbound", "no-such-claim"))).To(Succeed())
		}()

		share, err := reconcileOnce(&ShareReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: &fakePlacer{}}, "unbound")
		Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(share.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonWaitingForVolume))
	})

	It("places the share on the resolved node and waits for the agent", func() {
		boundClaim("media", "pvc-media-1")
		Expect(k8sClient.Create(ctx, newShare("media", "media"))).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, newShare("media", "media"))).To(Succeed())
		}()

		placer := &fakePlacer{node: "node-a", device: "/dev/drbd1000"}
		share, err := reconcileOnce(&ShareReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}, "media")
		Expect(err).NotTo(HaveOccurred())

		Expect(share.Status.Node).To(Equal("node-a"))
		Expect(share.Status.Device).To(Equal("/dev/drbd1000"))
		cond := meta.FindStatusCondition(share.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonWaitingForAgent))

		By("going available once the agent reports HostReady")
		meta.SetStatusCondition(&share.Status.Conditions, metav1.Condition{
			Type:   storagev1alpha1.ConditionHostReady,
			Status: metav1.ConditionTrue,
			Reason: storagev1alpha1.ReasonReady,
		})
		Expect(k8sClient.Status().Update(ctx, share)).To(Succeed())

		share, err = reconcileOnce(&ShareReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}, "media")
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(share.Status.Conditions, storagev1alpha1.ConditionAvailable)).To(BeTrue())
	})

	It("surfaces placement errors and retries", func() {
		boundClaim("erring", "pvc-erring-1")
		Expect(k8sClient.Create(ctx, newShare("erring", "erring"))).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, newShare("erring", "erring"))).To(Succeed())
		}()

		placer := &fakePlacer{err: fmt.Errorf("controller unreachable")}
		share, err := reconcileOnce(&ShareReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}, "erring")
		Expect(err).To(HaveOccurred())

		cond := meta.FindStatusCondition(share.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonLinstorError))
	})

	It("moves the share off an unready node and resets HostReady", func() {
		setNode(ctx, "node-a", true)
		setNode(ctx, "node-b", true)
		defer setNode(ctx, "node-a", true)

		boundClaim("failover", "pvc-failover-1")
		Expect(k8sClient.Create(ctx, newShare("failover", "failover"))).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, newShare("failover", "failover"))).To(Succeed())
		}()

		placer := &fakePlacer{node: "node-a", fallback: "node-b", device: "/dev/drbd1000"}
		r := &ShareReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: placer}
		share, err := reconcileOnce(r, "failover")
		Expect(err).NotTo(HaveOccurred())
		Expect(share.Status.Node).To(Equal("node-a"))

		By("the agent reporting the export up")
		meta.SetStatusCondition(&share.Status.Conditions, metav1.Condition{
			Type:   storagev1alpha1.ConditionHostReady,
			Status: metav1.ConditionTrue,
			Reason: storagev1alpha1.ReasonReady,
		})
		Expect(k8sClient.Status().Update(ctx, share)).To(Succeed())

		By("node-a going NotReady")
		setNode(ctx, "node-a", false)
		share, err = reconcileOnce(r, "failover")
		Expect(err).NotTo(HaveOccurred())
		Expect(share.Status.Node).To(Equal("node-b"))
		Expect(meta.IsStatusConditionTrue(share.Status.Conditions, storagev1alpha1.ConditionHostReady)).To(BeFalse())
		cond := meta.FindStatusCondition(share.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonWaitingForAgent))
	})

	It("rejects claims not backed by LINSTOR CSI", func() {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-hostpath"},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/x"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pv)).To(Succeed())
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeName:  "pv-hostpath",
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Create(ctx, newShare("foreign", "foreign"))).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, newShare("foreign", "foreign"))).To(Succeed())
		}()

		share, err := reconcileOnce(&ShareReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Linstor: &fakePlacer{}}, "foreign")
		Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(share.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonInvalidSpec))
	})
})
