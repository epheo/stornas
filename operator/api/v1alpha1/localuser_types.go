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

// LocalUserSpec defines the desired state of LocalUser.
// One user object feeds both UI login and the samba passdb.
type LocalUserSpec struct {
	// role scopes what the UI session may do.
	// +required
	// +kubebuilder:validation:Enum=admin;viewer
	Role string `json:"role"`

	// smb provisions the user in the samba passdb.
	// +optional
	// +kubebuilder:default=false
	SMB bool `json:"smb,omitempty"`

	// passwordSecretRef names a Secret with a password key.
	// +required
	PasswordSecretRef string `json:"passwordSecretRef"`
}

// LocalUserPassword returns the password a Secret's data holds; ok is
// false when it is empty. The operator reports and the agent revokes on
// this one rule.
func LocalUserPassword(data map[string][]byte) (password string, ok bool) {
	password = string(data["password"])
	return password, password != ""
}

// LocalUserStatus defines the observed state of LocalUser.
type LocalUserStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// passwordSecretVersion is the resourceVersion of the password Secret
	// once it holds a password. Only the operator watches Secrets (the
	// agent may only get them), so a rotation reaches the agent as a
	// change here.
	// +optional
	PasswordSecretVersion string `json:"passwordSecretVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="SMB",type=boolean,JSONPath=`.spec.smb`

// LocalUser is the Schema for the localusers API
type LocalUser struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of LocalUser
	// +required
	Spec LocalUserSpec `json:"spec"`

	// status defines the observed state of LocalUser
	// +optional
	Status LocalUserStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LocalUserList contains a list of LocalUser
type LocalUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LocalUser `json:"items"`
}

func init() {
	register(&LocalUser{}, &LocalUserList{})
}
