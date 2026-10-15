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

// Package v1alpha1 mirrors the subset of the r-drbd.io API stornas reads
// and writes. r-drbd owns the CRDs (image/manifests/r-drbd/05-crds.yaml);
// fields stornas does not use are left out, so these types are only ever
// created whole (Pool) or merge-patched (Volume), never updated.
// +kubebuilder:object:generate=true
// +kubebuilder:skip
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "r-drbd.io", Version: "v1alpha1"}

	schemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Pool{}, &PoolList{}, &Volume{}, &VolumeList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = schemeBuilder.AddToScheme
)

// Shared names (r-drbd crates/model/src/keys.rs and condition.rs).
const (
	Driver = "r-drbd.io"

	// StorageClass parameters.
	ParamTier     = "tier"
	ParamReplicas = "replicas"

	ConditionReady      = "Ready"
	ConditionDegraded   = "Degraded"
	ConditionQuorumLost = "QuorumLost"
	ConditionSplitBrain = "SplitBrain"
	ConditionAvailable  = "Available"
)
