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

// NFSExport configures the kernel nfsd export of a Share.
type NFSExport struct {
	// clients in exports(5) form, e.g. "192.168.1.0/24(rw,no_root_squash)".
	// +required
	// +kubebuilder:validation:MinItems=1
	Clients []string `json:"clients"`
}

// SMBExport configures the samba share of a Share.
type SMBExport struct {
	// name of the SMB share; defaults to the Share's name.
	// +optional
	Name string `json:"name,omitempty"`

	// validUsers references LocalUser names with smb enabled.
	// +optional
	ValidUsers []string `json:"validUsers,omitempty"`
}

// ShareSpec defines the desired state of Share.
// +kubebuilder:validation:XValidation:rule="has(self.nfs) || has(self.smb)",message="at least one of nfs or smb must be set"
type ShareSpec struct {
	// claimName of a filesystem-mode PVC in the Share's namespace.
	// +required
	ClaimName string `json:"claimName"`

	// +optional
	NFS *NFSExport `json:"nfs,omitempty"`

	// +optional
	SMB *SMBExport `json:"smb,omitempty"`
}

// ShareStatus defines the observed state of Share.
type ShareStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// node and device are the operator's placement decision: where the
	// backing LINSTOR resource is (or should become) primary and which
	// block device the agent mounts there. The agent only executes.
	// +optional
	Node string `json:"node,omitempty"`
	// +optional
	Device string `json:"device,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Pending;Exported;Failed
	State string `json:"state,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.spec.claimName`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.node`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`

// Share is the Schema for the shares API
type Share struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Share
	// +required
	Spec ShareSpec `json:"spec"`

	// status defines the observed state of Share
	// +optional
	Status ShareStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ShareList contains a list of Share
type ShareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Share `json:"items"`
}

func init() {
	register(&Share{}, &ShareList{})
}
