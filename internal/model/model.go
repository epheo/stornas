// Package model holds the UI-facing read types. tygo generates
// web/src/lib/model.gen.ts from this package (make types), so every field
// here is part of the wire contract with the frontend.
package model

// Pool is one StoragePool flattened for the UI: spec identity, agent-observed
// host state, and the operator's availability verdict in one row.
type Pool struct {
	Name    string   `json:"name"`
	Node    string   `json:"node"`
	Raid    string   `json:"raid"`
	Devices []Device `json:"devices"`

	VG            string `json:"vg"`
	CapacityBytes int64  `json:"capacityBytes"`
	FreeBytes     int64  `json:"freeBytes"`
	Health        string `json:"health"` // Online | Degraded | Failed | Unknown

	Available bool   `json:"available"`
	Reason    string `json:"reason"` // Available condition reason; explains false
	Linstor   string `json:"linstor"`
}

// Device is one backing disk's observed state.
type Device struct {
	Path  string `json:"path"`
	State string `json:"state"` // InSync | Rebuilding | Failed | Missing
	Smart string `json:"smart"`
}

// Node is one cluster member.
type Node struct {
	Name           string   `json:"name"`
	Ready          bool     `json:"ready"`
	Roles          []string `json:"roles"`
	Addresses      []string `json:"addresses"`
	KubeletVersion string   `json:"kubeletVersion"`
	Disks          []Disk   `json:"disks"`
}

// Disk is one observed block device on a node; unclaimed disks feed the
// create-pool form.
type Disk struct {
	Path       string `json:"path"`
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	SizeBytes  int64  `json:"sizeBytes"`
	Rotational bool   `json:"rotational"`
	Claimed    bool   `json:"claimed"`
}

// Volume is one PVC: the unit everything else references (LUNs, shares,
// VM disks all point at PVCs).
type Volume struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	StorageClass  string `json:"storageClass"`
	Phase         string `json:"phase"`
	CapacityBytes int64  `json:"capacityBytes"`
	Block         bool   `json:"block"`
}

// Share is one NFS/SMB export: spec identity plus the operator's placement
// and the agent's export state.
type Share struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Claim     string `json:"claim"`
	NFS       bool   `json:"nfs"`
	SMB       bool   `json:"smb"`
	Node      string `json:"node"`
	State     string `json:"state"` // Pending | Exported | Failed
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// Snapshot is one consistent frame of everything the UI shows; the WS hub
// sends a full frame per change rather than diffs.
type Snapshot struct {
	Pools   []Pool   `json:"pools"`
	Nodes   []Node   `json:"nodes"`
	Volumes []Volume `json:"volumes"`
	Shares  []Share  `json:"shares"`
}
