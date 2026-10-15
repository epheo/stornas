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
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

// claimVolume resolves a PVC to its r-drbd Volume, whose name is the CSI
// volume handle; shared by the Share and Target reconcilers. A non-empty
// reason means not usable yet.
func claimVolume(ctx context.Context, c client.Reader, namespace, claim string) (vol *rdrbdv1alpha1.Volume, reason, msg string) {
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claim}, &pvc); err != nil {
		return nil, storagev1alpha1.ReasonWaitingForVolume, "claim " + claim + " not found"
	}
	if pvc.Spec.VolumeName == "" {
		return nil, storagev1alpha1.ReasonWaitingForVolume, "claim " + claim + " not bound"
	}
	var pv corev1.PersistentVolume
	if err := c.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return nil, storagev1alpha1.ReasonWaitingForVolume, "volume " + pvc.Spec.VolumeName + " not found"
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != rdrbdv1alpha1.Driver {
		return nil, storagev1alpha1.ReasonInvalidSpec, "claim is not backed by r-drbd"
	}
	vol = &rdrbdv1alpha1.Volume{}
	if err := c.Get(ctx, types.NamespacedName{Name: pv.Spec.CSI.VolumeHandle}, vol); err != nil {
		return nil, storagev1alpha1.ReasonWaitingForVolume, "r-drbd volume " + pv.Spec.CSI.VolumeHandle + " not found"
	}
	return vol, "", ""
}

// volumeClaim maps a Volume event to the claim bound to it: the PV keeps
// the Volume's name and records its claim.
func volumeClaim(ctx context.Context, c client.Reader, obj client.Object) (types.NamespacedName, bool) {
	var pv corev1.PersistentVolume
	if err := c.Get(ctx, types.NamespacedName{Name: obj.GetName()}, &pv); err != nil || pv.Spec.ClaimRef == nil {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: pv.Spec.ClaimRef.Namespace, Name: pv.Spec.ClaimRef.Name}, true
}

// servingKey is what placement reads from a Volume.
type servingKey struct {
	primary  string
	roles    map[string]rdrbdv1alpha1.Role
	replicas map[string]rdrbdv1alpha1.ReplicaStatus
}

func serving(v *rdrbdv1alpha1.Volume) servingKey {
	k := servingKey{primary: v.Status.Primary, roles: map[string]rdrbdv1alpha1.Role{}, replicas: map[string]rdrbdv1alpha1.ReplicaStatus{}}
	if v.Spec.Assignment != nil {
		for _, r := range v.Spec.Assignment.Replicas {
			k.roles[r.Node] = r.Role
		}
	}
	for _, r := range v.Status.Replicas {
		k.replicas[r.Node] = rdrbdv1alpha1.ReplicaStatus{Node: r.Node, Present: r.Present, InUse: r.InUse, DevicePath: r.DevicePath}
	}
	return k
}

// volumeServingChanged passes Volume updates that can move placement:
// primary, roles, presence, devices. Sync progress and peer states
// churn without changing who serves.
var volumeServingChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldVol, okOld := e.ObjectOld.(*rdrbdv1alpha1.Volume)
		newVol, okNew := e.ObjectNew.(*rdrbdv1alpha1.Volume)
		return okOld && okNew && !reflect.DeepEqual(serving(oldVol), serving(newVol))
	},
}
