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

	reconcileOnce := func(name string) *storagev1alpha1.LocalUser {
		r := &LocalUserReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
		Expect(err).NotTo(HaveOccurred())
		user := &storagev1alpha1.LocalUser{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, user)).To(Succeed())
		return user
	}

	newUser := func(name, secretRef string) *storagev1alpha1.LocalUser {
		return &storagev1alpha1.LocalUser{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       storagev1alpha1.LocalUserSpec{Role: "admin", SMB: true, PasswordSecretRef: secretRef},
		}
	}

	It("flags a missing or empty password secret", func() {
		Expect(k8sClient.Create(ctx, newUser("alice", "alice-password"))).To(Succeed())
		defer func() {
			Expect(k8sClient.Delete(ctx, newUser("alice", "alice-password"))).To(Succeed())
		}()

		user := reconcileOnce("alice")
		cond := meta.FindStatusCondition(user.Status.Conditions, storagev1alpha1.ConditionAvailable)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(storagev1alpha1.ReasonInvalidSpec))

		By("going available once the secret carries a password")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "alice-password", Namespace: "default"},
			Data:       map[string][]byte{"password": []byte("hunter2")},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		user = reconcileOnce("alice")
		Expect(meta.IsStatusConditionTrue(user.Status.Conditions, storagev1alpha1.ConditionAvailable)).To(BeTrue())
	})
})
