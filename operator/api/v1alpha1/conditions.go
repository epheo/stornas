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
	ConditionAvailable         = "Available"
	ConditionHostReady         = "HostReady"
	ConditionLinstorRegistered = "LinstorRegistered"
)

const (
	ReasonReady            = "Ready"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonWaitingForAgent  = "WaitingForAgent"
	ReasonWaitingForVolume = "WaitingForVolume"
	ReasonHostError        = "HostError"
	ReasonNotConfigured    = "NotConfigured"
	ReasonLinstorError     = "LinstorError"
)

// LinstorPool is the single LINSTOR storage pool name every StoragePool
// registers under, so StorageClasses stay node agnostic (image/manifests).
const LinstorPool = "stornas"

// ThinLV is the thin pool LV inside every pool's VG.
const ThinLV = "thin"

// IQNPrefix is shared by the operator (status.iqn) and the agent (teardown
// by name after the Target is gone); one constant keeps them in step.
const IQNPrefix = "iqn.2026-08.io.stornas:"

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
