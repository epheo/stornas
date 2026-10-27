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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var _ = Describe("LocalUser Controller", func() {
	ctx := context.Background()

	reconcileOnce := func(name string) (*storagev1alpha1.LocalUser, reconcile.Result) {
		r := &LocalUserReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
		Expect(err).NotTo(HaveOccurred())
		user := &storagev1alpha1.LocalUser{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, user)).To(Succeed())
		return user, res
	}

	newUser := func(name, secretRef string) *storagev1alpha1.LocalUser {
		return &storagev1alpha1.LocalUser{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       storagev1alpha1.LocalUserSpec{Role: "admin", SMB: true, PasswordSecretRef: secretRef},
		}
	}

	It("flags a missing or empty password secret and stamps a usable one", func() {
		Expect(k8sClient.Create(ctx, newUser("alice", "alice-password"))).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, newUser("alice", "alice-password"))).To(Succeed())
		}()

		user, res := reconcileOnce("alice")
		cond := meta.FindStatusCondition(user.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonInvalidSpec))
		Expect(user.Status.PasswordSecretVersion).To(BeEmpty())
		// The Secret watch brings the fix; no poll.
		Expect(res.RequeueAfter).To(BeZero())

		By("still invalid while the secret has no password key")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "alice-password", Namespace: "default"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, secret)).To(Succeed()) }()
		r := &LocalUserReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(r.secretUsers(ctx, secret)).To(
			ConsistOf(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "alice"}}))

		user, _ = reconcileOnce("alice")
		cond = meta.FindStatusCondition(user.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(user.Status.PasswordSecretVersion).To(BeEmpty())

		By("going available once the secret carries a password")
		secret.Data = map[string][]byte{"password": []byte("hunter2")}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		user, _ = reconcileOnce("alice")
		Expect(meta.IsStatusConditionTrue(user.Status.Conditions, storagev1alpha1.ConditionAvailable)).To(BeTrue())
		Expect(user.Status.PasswordSecretVersion).To(Equal(secret.ResourceVersion))

		By("restamping on rotation, which is what wakes the agents")
		secret.Data["password"] = []byte("rotated1")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		user, _ = reconcileOnce("alice")
		Expect(user.Status.PasswordSecretVersion).To(Equal(secret.ResourceVersion))
	})
})
