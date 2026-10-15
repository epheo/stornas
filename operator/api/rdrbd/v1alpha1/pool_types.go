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

// PoolSpec is immutable: a pool is a fixed thin pool on a fixed node.
type PoolSpec struct {
	Node     string `json:"node"`
	Tier     string `json:"tier"`
	VG       string `json:"vg"`
	ThinPool string `json:"thinPool"`
}

type PoolStatus struct {
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`
	// +optional
	FreeBytes *int64 `json:"freeBytes,omitempty"`
	// volumes with a diskful replica here; the r-drbd finalizer holds
	// deletion while it is non-empty.
	// +optional
	Volumes []string `json:"volumes,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

type Pool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PoolSpec `json:"spec"`
	// +optional
	Status PoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Pool `json:"items"`
}
