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

// Condition types. HostReady is written by the node agent, the rest only
// by the operator, so the two status writers never fight over an entry.
// One exception: the operator resets HostReady when placement moves,
// because the value left behind describes the losing node.
const (
	ConditionAvailable = "Available"
	ConditionHostReady = "HostReady"
	// Registered mirrors the r-drbd Pool's Available condition.
	ConditionRegistered = "Registered"
	// TornDown is the agent's confirmation that a deleting pool's host
	// state (VG, array, signatures) is gone; it releases the operator's
	// finalizer.
	ConditionTornDown = "TornDown"
	// Deregistered is the operator's clearance for the host wipe: set only
	// once r-drbd released the pool's Pool, so the agent never
	// wipes a VG that still backs volumes.
	ConditionDeregistered = "Deregistered"
)

const (
	ReasonReady            = "Ready"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonWaitingForAgent  = "WaitingForAgent"
	ReasonWaitingForVolume = "WaitingForVolume"
	ReasonHostError        = "HostError"
	ReasonNotConfigured    = "NotConfigured"
	ReasonWaitingForPool   = "WaitingForPool"
	ReasonBackendError     = "BackendError"
	ReasonUnsupported      = "Unsupported"
)

// DefaultTier is the tier of the shipped StorageClasses (image/manifests);
// other tiers get operator-made classes labelled with TierClassLabel.
const (
	DefaultTier     = "default"
	LocalClass      = "stornas-local"
	ReplicatedClass = "stornas-replicated"
	TierClassLabel  = "storage.stornas.io/tier"
)

// PoolAnnotation pins a claim to one StoragePool: the API stamps it from
// the volume form and r-drbd's CSI controller places the volume in the
// r-drbd Pool of that name, which the operator keeps equal to the
// StoragePool name.
const PoolAnnotation = "r-drbd.io/pool"

// TierClasses names the local and replicated StorageClasses of a tier.
func TierClasses(tier string) (local, replicated string) {
	if tier == "" || tier == DefaultTier {
		return LocalClass, ReplicatedClass
	}
	return LocalClass + "-" + tier, ReplicatedClass + "-" + tier
}

// ThinLV is the thin pool LV inside every pool's VG.
const ThinLV = "thin"

// IQNPrefix is shared by the operator (status.iqn) and the agent (teardown
// by name after the Target is gone); one constant keeps them in step.
const IQNPrefix = "iqn.2026-08.io.stornas:"

// ConsumerAnnotation declares who will consume a PVC. The stornas API
// stamps ConsumerHost on volumes it creates: their consumer is kernel
// nfsd or LIO, never a pod.
const (
	ConsumerAnnotation = "storage.stornas.io/consumer"
	ConsumerHost       = "host"
)

// VGName prefixes the CR name so appliance VGs are distinguishable from
// any the user created by hand; the agent never touches foreign VGs.
func (p *StoragePool) VGName() string {
	return "stornas-" + p.Name
}

// MinDevices is the device count floor per raid level.
func MinDevices(raid string) int {
	switch raid {
	case "raid1":
		return 2
	case "raid5":
		return 3
	case "raid10":
		return 4
	default:
		return 1
	}
}
