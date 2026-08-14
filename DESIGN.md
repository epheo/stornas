# stornas design

Status: draft for review, 2026-08-04.
Successor to FreeNAS for homelab NAS/SAN and hyperconverged KubeVirt storage.
Not aiming at massive scale.
Better than TrueNAS at SAN, replication, and KubeVirt; simpler everywhere else.

## Locked decisions

- Base: epheo/microshift distro (CentOS Stream 10, bootc, from-source MicroShift).
- Topology: single node by default, single master plus workers when grown.
- LVM everywhere; Stratis dropped.
- LINSTOR plus Piraeus operator for volume orchestration; no homegrown DRBD control.
- LINSTOR CSI replaces TopoLVM in the distro.
- DRBD 9 kmod built in CI, staged into the bootc image, gated by greenboot.
- DRBD protocol C default, A offered for async remote peers, B not exposed.
- Replication is a StorageClass choice, never an install mode.
- Custom web UI; Cockpit rejected.
- Chassis copied from dotvirt; GitOps core of dotvirt not reused.
- stornas is the storage plane only; dotvirt owns the VM plane.

## Self-contained image

An appliance must boot and serve storage with zero external connectivity.
Everything ships inside the bootc image:

- stornas server, agent, and operator images, embedded in the image's
  container storage via the distro's embedding mechanism (same path the
  distro uses for its edge components).
- Piraeus operator, LINSTOR controller/satellite, and CSI images, embedded
  and digest-pinned the same way.
- The DRBD 9 kmod, matched to the image kernel at build time.
- All manifests, applied from disk, never fetched.
- The SPA, served by the stornas server from the image.

No registry pull, package install, or download happens at runtime.
Updates arrive as a new bootc image; greenboot gates, bootc rolls back.
Greenboot requires the piraeus operator, linstor-controller, and the
stornas operator, server, and agent; the distro's TopoLVM check is
erased with its RPM.

## Storage planes

Block plane, SAN and PVC:
devices, then LVM raid, then LVM thin, then optional DRBD, then CSI or LIO.

File plane, NAS:
a filesystem PVC (XFS) from the same CSI, exported by NFS or SMB.

One rule keeps the design small: every volume is a PVC.
SAN LUNs and NAS shares reference PVCs in the stornas namespace.
KubeVirt consumes PVCs directly.
Snapshots, clones, and resize come from the CSI path once, for everything.

## StorageClasses

- `stornas-local`: LVM thin only, no DRBD layer, zero overhead. Default.
- `stornas-replicated`: DRBD layer, placeCount 2 or 3, protocol C.
- `stornas-remote`: protocol A to a designated async peer. Post-v1.

A local volume can gain the DRBD layer later when a second node joins.

## Components

Host image additions (bootc layer on the microshift distro):
- drbd9 kmod (CI-built per kernel), drbd-utils
- lvm2, mdadm, smartmontools, nvme-cli
- targetcli/rtslib (or configfs driven directly)
- samba, nfs-utils (kernel nfsd)

Cluster workloads:
- Piraeus operator, LINSTOR controller on master, satellite per node.
- stornas-operator: reconciles the CRDs below.
- stornas-agent: privileged DaemonSet, the only thing that touches the host.
- stornas server: API plus web UI, runs on master.

The agent stays dumb: it reconciles host state from CRD specs.
It renders samba and exports config, drives configfs for LIO,
creates md/VG on pool creation, reports device inventory and SMART.
All decisions live in the operator.

## CRDs (storage.stornas.io/v1alpha1)

### StoragePool

Pools are node local.
The operator registers each pool as a LINSTOR storage pool on that satellite.

```yaml
kind: StoragePool
metadata:
  name: tank-node-a
spec:
  node: node-a
  devices: [/dev/disk/by-id/wwn-0x5000c500a1b2c3d4, /dev/disk/by-id/wwn-0x5000c500e5f6a7b8]
  raid: raid1        # none | raid1 | raid5 | raid10
  thin: true
status:
  vg: stornas-tank
  linstorPool: tank
  capacity: 4Ti
  free: 3.2Ti
  health: Online     # Online | Degraded | Failed
  devices:
    - {path: ..., serial: ..., smart: Passed, state: InSync}
```

Raid lives in mdadm below the PV: disks form one md array, the VG sits
on it, and the thin pool stays linear.
Each layer has one owner; LINSTOR only ever sees a linear thin pool on a
single plain device, the configuration its containerized satellite can
always activate.
The md state model (active, faulty, spare rebuilding, /proc/mdstat
progress) maps directly onto device status and rebuildPercent.

Pool deletion is refused while volumes remain, then finalizer-chained:
LINSTOR deregisters, the agent dismantles the host state (VG, array,
member signatures) and confirms, only then does the CR go. Disks read
unclaimed again and are reusable from the UI.

### Target (iSCSI)

```yaml
kind: Target
metadata:
  name: vms
spec:
  vip: 192.168.1.50/24        # required when any LUN is replicated
  luns:
    - id: 0
      claimName: vm-disk-0    # PVC, block mode
  initiators:
    - iqn: iqn.1994-05.com.redhat:client1
      chapSecretRef: client1-chap
status:
  iqn: iqn.2026-08.io.stornas:vms
  activeNode: node-a
  sessions: 1
  state: Exported
```

Failover model, v1: active/passive.
The operator places the target on the DRBD primary.
The agent raises the VIP with gratuitous ARP; initiators reconnect.
No dual-head ALUA, no persistent reservation handover in v1.

### Share (NAS)

```yaml
kind: Share
metadata:
  name: media
spec:
  claimName: media            # PVC, filesystem mode, XFS
  nfs:
    clients: ["192.168.1.0/24(rw,no_root_squash)"]
  smb:
    name: media
    validUsers: [alice]
status:
  node: node-a
  state: Exported
```

NFS clients mount server:/<share-name>: the shares directory is the
fsid=0 pseudo root, because the composefs rootfs cannot anchor the
NFSv4 tree. v4 only; v3 ports stay closed.

SMB uses the shared-folder model: valid users gate access, and inside
the share every user acts as one owner (force user root), matching
NFS's no_root_squash default. Per-user ownership and ACLs are post-v1.

### LocalUser

```yaml
kind: LocalUser
metadata:
  name: alice
spec:
  role: admin                 # admin | viewer, UI role
  smb: true
  passwordSecretRef: alice-password
```

One user object feeds both UI login and the samba passdb.

## UI and API

Chassis copied from dotvirt:
SvelteKit, Svelte 5, Tailwind, tygo typegen,
eventbus, clusterstate, reflect, stream, restfactory.

Reflectors watch stornas CRDs, PVCs, LINSTOR state, and events.
The WS hub streams pool health, resync progress, and sessions from cache.
Mutations write CRDs under the caller's identity.

Auth differs from dotvirt: appliance users have no kubeconfig.
First boot generates an admin password, printed to console and MOTD.
Session cookie in front, backend impersonates a role-scoped ServiceAccount.
OIDC later, not v1.

v1 pages: dashboard, pools, volumes (PVCs), shares, targets, nodes, alerts.
Read paths first, mutations behind them.

## Failure matrix

| Failure | Behavior | Operator/user action |
|---|---|---|
| Disk dies in raid1 pool | Pool Degraded, IO continues | Alert; replace flow in UI |
| Node dies, replicated volume | Peer keeps IO; target VIP moves; PVC reattaches | Auto resync on return |
| Node dies, local volume | Volume unavailable until node returns | Stated plainly in UI |
| Master dies, workers alive | Data IO continues; no provisioning; UI down | Restore master |
| 2-node split brain | DRBD quorum lost; IO suspended on both | Manual pick-survivor flow in UI |
| Kernel bump vs kmod mismatch | greenboot fails; bootc rolls back | CI must never publish mismatched image |
| LINSTOR controller down | IO fine; create/resize/snapshot blocked | Controller restarts on master |

Two-node clusters cannot have quorum.
v1 answer: suspend on split, manual resolution in the UI.
Documented upgrade: a third machine running only a diskless tiebreaker satellite.

## v1 scope

1. Image: distro plus LINSTOR, kmod CI stage, smartd, samba, nfsd, LIO.
2. Storage: stornas-local and stornas-replicated classes.
3. NAS: NFS and SMB shares via Share CRD.
4. SAN: iSCSI targets; replicated LUN failover active/passive with VIP.
5. UI: the six pages above, admin/viewer roles, first-boot password.

Out of v1:
ALUA multipath, protocol A remote replication, FC/NVMe-oF,
scheduled snapshots, quotas, OIDC, controller HA, any VM feature.

## Repo layout

```
cmd/stornas/         API + UI server
cmd/stornas-agent/   node DaemonSet
internal/            copied chassis + pool/target/share logic
web/                 SvelteKit app
operator/            kubebuilder module: CRDs + controllers
image/               bootc layer, kmod build, manifests
hack/
```

## Open questions

- LVM raid vs mdadm: decided 2026-08-13, mdadm under the PV. The LINSTOR
  satellite cannot activate a raid-backed thin pool from its container
  (rmeta create ioctl: busy), and md is the battle-worn path every NAS
  incumbent ships.
- VIP mechanics: decided, agent-managed ip addr plus GARP; no keepalived.
- NFS: kernel nfsd (proposal) vs ganesha in a pod.
- LIO: drive configfs from Go in the agent, or shell out to targetcli. Proposal: configfs.
- Snapshot scheduling: CSI VolumeSnapshot plus a cron field on the PVC page, post-v1.
- Does stornas-remote (protocol A) pair appliances outside the cluster, and who owns that config.
