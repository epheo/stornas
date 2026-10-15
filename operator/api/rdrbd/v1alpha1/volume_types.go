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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Volume names equal the CSI volume handle, which is the PV name.
type VolumeSpec struct {
	SizeBytes int64  `json:"sizeBytes"`
	Replicas  int32  `json:"replicas"`
	Tier      string `json:"tier"`
	// +optional
	Pool string `json:"pool,omitempty"`
	// +optional
	Node string `json:"node,omitempty"`
	// +optional
	Resolve *Resolve `json:"resolve,omitempty"`
	// +optional
	Assignment *Assignment `json:"assignment,omitempty"`
}

// Resolve discards every diskful replica except the survivor's.
type Resolve struct {
	Survivor string `json:"survivor"`
}

type Assignment struct {
	Replicas []AssignedReplica `json:"replicas"`
}

type Role string

const (
	RoleDiskful    Role = "diskful"
	RoleDiskless   Role = "diskless"
	RoleTiebreaker Role = "tiebreaker"
)

type AssignedReplica struct {
	Node string `json:"node"`
	// +optional
	Pool   string `json:"pool,omitempty"`
	NodeID int32  `json:"nodeId"`
	Role   Role   `json:"role"`
}

type VolumeStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// primary is where DRBD is Primary, or where the LV is open for replicas 1.
	// +optional
	Primary string `json:"primary,omitempty"`
	// +optional
	Replicas []ReplicaStatus `json:"replicas,omitempty"`
}

// ReplicaStatus is written by that node's agent.
type ReplicaStatus struct {
	Node string `json:"node"`
	// +optional
	Present bool `json:"present,omitempty"`
	// +optional
	DiskState string `json:"diskState,omitempty"`
	// +optional
	SyncPercent *int32 `json:"syncPercent,omitempty"`
	// peers maps a peer node to its DRBD connection state.
	// +optional
	Peers map[string]string `json:"peers,omitempty"`
	// +optional
	InUse bool `json:"inUse,omitempty"`
	// +optional
	Quorum *bool `json:"quorum,omitempty"`
	// +optional
	DevicePath string `json:"devicePath,omitempty"`
}

// +kubebuilder:object:root=true

type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec VolumeSpec `json:"spec"`
	// +optional
	Status VolumeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type VolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Volume `json:"items"`
}

// Diskful reports whether node holds a diskful replica in the assignment.
func (v *Volume) Diskful(node string) bool {
	if v.Spec.Assignment == nil {
		return false
	}
	for _, r := range v.Spec.Assignment.Replicas {
		if r.Node == node && r.Role == RoleDiskful {
			return true
		}
	}
	return false
}
