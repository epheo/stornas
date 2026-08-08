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
// VM disks all point at PVCs). Resource is the bound PV name, which is
// also the LINSTOR resource name for CSI-provisioned volumes.
type Volume struct {
	Namespace     string       `json:"namespace"`
	Name          string       `json:"name"`
	StorageClass  string       `json:"storageClass"`
	Phase         string       `json:"phase"`
	CapacityBytes int64        `json:"capacityBytes"`
	Block         bool         `json:"block"`
	Resource      string       `json:"resource"`
	Replication   *Replication `json:"replication"`
}

// Replication is the DRBD view of one volume, absent for non-replicated
// or unresolved volumes.
type Replication struct {
	Replicas []Replica `json:"replicas"`
}

// Replica is one node's copy: disk state (UpToDate, Inconsistent,
// SyncTarget...) and whether the node currently holds the resource open.
// SyncPercent is set only while a resync runs, in whole percent so a
// running sync does not push a frame per decimal tick.
type Replica struct {
	Node        string `json:"node"`
	DiskState   string `json:"diskState"`
	InUse       bool   `json:"inUse"`
	SyncPercent *int   `json:"syncPercent"`
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

// Target is one iSCSI target: spec identity plus the operator's placement
// and the agent's LIO state.
type Target struct {
	Namespace  string      `json:"namespace"`
	Name       string      `json:"name"`
	IQN        string      `json:"iqn"`
	VIP        string      `json:"vip"`
	ActiveNode string      `json:"activeNode"`
	Sessions   int32       `json:"sessions"`
	State      string      `json:"state"` // Pending | Exported | Failed
	LUNs       []TargetLUN `json:"luns"`
	Available  bool        `json:"available"`
	Reason     string      `json:"reason"`
}

// TargetLUN is one logical unit: the claim it exposes and the device the
// operator resolved for the agent.
type TargetLUN struct {
	ID     int32  `json:"id"`
	Claim  string `json:"claim"`
	Device string `json:"device"`
}

// VolumeSnapshot is one CSI snapshot of a volume.
type VolumeSnapshot struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Source    string `json:"source"` // source PVC name
	Ready     bool   `json:"ready"`
	SizeBytes int64  `json:"sizeBytes"`
	CreatedAt string `json:"createdAt"` // RFC3339; empty until bound
}

// Alert is one Warning event: the appliance's trouble feed. Kubernetes
// aggregates repeats, so Count and LastSeen carry the recurrence story.
type Alert struct {
	Namespace string `json:"namespace"`
	Object    string `json:"object"` // Kind/name of the involved object
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Count     int32  `json:"count"`
	LastSeen  string `json:"lastSeen"` // RFC3339
}

// Snapshot is one consistent frame of everything the UI shows; the WS hub
// sends a full frame per change rather than diffs.
type Snapshot struct {
	Pools     []Pool           `json:"pools"`
	Nodes     []Node           `json:"nodes"`
	Volumes   []Volume         `json:"volumes"`
	Shares    []Share          `json:"shares"`
	Targets   []Target         `json:"targets"`
	Snapshots []VolumeSnapshot `json:"snapshots"`
	Alerts    []Alert          `json:"alerts"`
}
