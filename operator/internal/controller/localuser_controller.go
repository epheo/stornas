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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// LocalUserReconciler validates the referenced password Secret; the agent
// consumes it directly for the samba passdb, the UI session layer later.
type LocalUserReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// secretSettleInterval paces re-checks of a missing or empty password
// Secret: nothing watches Secrets, so a fix would otherwise go unseen
// until the LocalUser itself is touched.
const secretSettleInterval = 15 * time.Second

// +kubebuilder:rbac:groups=storage.stornas.io,resources=localusers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.stornas.io,resources=localusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.stornas.io,resources=localusers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *LocalUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var user storagev1alpha1.LocalUser
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	available := metav1.Condition{
		Type:               storagev1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             storagev1alpha1.ReasonReady,
		ObservedGeneration: user.Generation,
	}
	res := ctrl.Result{}
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: user.Namespace, Name: user.Spec.PasswordSecretRef}, &secret)
	switch {
	case err != nil:
		available.Status = metav1.ConditionFalse
		available.Reason = storagev1alpha1.ReasonInvalidSpec
		available.Message = "password secret " + user.Spec.PasswordSecretRef + " not found"
		res.RequeueAfter = secretSettleInterval
	case len(secret.Data["password"]) == 0:
		available.Status = metav1.ConditionFalse
		available.Reason = storagev1alpha1.ReasonInvalidSpec
		available.Message = "password secret " + user.Spec.PasswordSecretRef + " has no password key"
		res.RequeueAfter = secretSettleInterval
	}

	meta.SetStatusCondition(&user.Status.Conditions, available)
	if err := r.Status().Update(ctx, &user); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	return res, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LocalUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.LocalUser{}).
		Named("localuser").
		Complete(r)
}
