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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StoragePoolSpec defines the desired state of StoragePool.
// Pools are node local; the operator registers each pool as a LINSTOR
// storage pool on that node's satellite.
type StoragePoolSpec struct {
	// node that owns the devices; immutable once set.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="node is immutable"
	Node string `json:"node"`

	// devices by stable path (/dev/disk/by-id/...). Members may be swapped
	// one for one (the disk replace flow); the count is fixed so the raid
	// geometry never changes under a live pool.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:XValidation:rule="size(self) == size(oldSelf)",message="devices may be swapped one for one, not added or removed"
	Devices []string `json:"devices"`

	// raid level, implemented as LVM raid types.
	// +optional
	// +kubebuilder:validation:Enum=none;raid1;raid5;raid10
	// +kubebuilder:default=none
	Raid string `json:"raid,omitempty"`

	// thin enables an LVM thin pool; required for snapshots and clones.
	// +optional
	// +kubebuilder:default=true
	Thin *bool `json:"thin,omitempty"`
}

// DeviceStatus is the observed state of one backing device.
type DeviceStatus struct {
	Path string `json:"path"`
	// +optional
	Serial string `json:"serial,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Passed;Failed;Unknown
	Smart string `json:"smart,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=InSync;Rebuilding;Failed;Missing
	State string `json:"state,omitempty"`
}

// StoragePoolStatus defines the observed state of StoragePool.
type StoragePoolStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	VG string `json:"vg,omitempty"`
	// +optional
	LinstorPool string `json:"linstorPool,omitempty"`
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`
	// +optional
	Free *resource.Quantity `json:"free,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Online;Degraded;Failed
	Health string `json:"health,omitempty"`
	// +optional
	Devices []DeviceStatus `json:"devices,omitempty"`
	// rebuildPercent tracks an active raid repair or device evacuation;
	// absent when nothing is rebuilding.
	// +optional
	RebuildPercent *int32 `json:"rebuildPercent,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.node`
// +kubebuilder:printcolumn:name="Raid",type=string,JSONPath=`.spec.raid`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Capacity",type=string,JSONPath=`.status.capacity`
// +kubebuilder:printcolumn:name="Free",type=string,JSONPath=`.status.free`

// StoragePool is the Schema for the storagepools API
type StoragePool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of StoragePool
	// +required
	Spec StoragePoolSpec `json:"spec"`

	// status defines the observed state of StoragePool
	// +optional
	Status StoragePoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// StoragePoolList contains a list of StoragePool
type StoragePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []StoragePool `json:"items"`
}

func init() {
	register(&StoragePool{}, &StoragePoolList{})
}
