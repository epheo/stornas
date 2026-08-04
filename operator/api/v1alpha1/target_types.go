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

// LUN maps a block-mode PVC to an iSCSI logical unit.
type LUN struct {
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	ID int32 `json:"id"`

	// claimName of a block-mode PVC in the Target's namespace.
	// +required
	ClaimName string `json:"claimName"`
}

// Initiator grants access to one client IQN.
type Initiator struct {
	// +required
	IQN string `json:"iqn"`

	// chapSecretRef names a Secret with username/password keys; empty
	// disables CHAP for this initiator.
	// +optional
	ChapSecretRef string `json:"chapSecretRef,omitempty"`
}

// TargetSpec defines the desired state of Target.
// Failover is active/passive: the operator places the target on the DRBD
// primary and the agent raises the VIP there.
type TargetSpec struct {
	// vip in CIDR form; required when any LUN's PVC is replicated.
	// +optional
	VIP string `json:"vip,omitempty"`

	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=id
	LUNs []LUN `json:"luns"`

	// initiators allowed to log in; empty denies all.
	// +optional
	// +listType=map
	// +listMapKey=iqn
	Initiators []Initiator `json:"initiators,omitempty"`
}

// TargetStatus defines the observed state of Target.
type TargetStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	IQN string `json:"iqn,omitempty"`
	// +optional
	ActiveNode string `json:"activeNode,omitempty"`
	// +optional
	Sessions int32 `json:"sessions,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Pending;Exported;Failed
	State string `json:"state,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="IQN",type=string,JSONPath=`.status.iqn`
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.activeNode`
// +kubebuilder:printcolumn:name="Sessions",type=integer,JSONPath=`.status.sessions`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`

// Target is the Schema for the targets API
type Target struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Target
	// +required
	Spec TargetSpec `json:"spec"`

	// status defines the observed state of Target
	// +optional
	Status TargetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TargetList contains a list of Target
type TargetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Target `json:"items"`
}

func init() {
	register(&Target{}, &TargetList{})
}
