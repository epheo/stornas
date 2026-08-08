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

// NodeInventorySpec is intentionally empty: inventories are pure agent
// observations, named after the node, created and refreshed by the agent.
type NodeInventorySpec struct{}

// Disk is one block device the agent observed.
type Disk struct {
	// path is the stable identifier pools should reference
	// (/dev/disk/by-id/... when the device carries a WWN or serial).
	Path string `json:"path"`
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Serial string `json:"serial,omitempty"`
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
	// +optional
	Rotational bool `json:"rotational,omitempty"`
	// claimed marks disks already carrying an LVM PV; the create-pool UI
	// offers only unclaimed ones.
	// +optional
	Claimed bool `json:"claimed,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Passed;Failed;Unknown
	Smart string `json:"smart,omitempty"`
	// +optional
	TempCelsius *int32 `json:"tempCelsius,omitempty"`
	// +optional
	PowerOnHours *int64 `json:"powerOnHours,omitempty"`
}

// NodeInventoryStatus is the agent's latest disk observation.
type NodeInventoryStatus struct {
	// +optional
	Disks []Disk `json:"disks,omitempty"`
	// +optional
	ObservedAt metav1.Time `json:"observedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.status.observedAt`

// NodeInventory is the Schema for the nodeinventories API; one per node,
// named after it.
type NodeInventory struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +optional
	Spec NodeInventorySpec `json:"spec,omitzero"`

	// +optional
	Status NodeInventoryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NodeInventoryList contains a list of NodeInventory
type NodeInventoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NodeInventory `json:"items"`
}

func init() {
	register(&NodeInventory{}, &NodeInventoryList{})
}
