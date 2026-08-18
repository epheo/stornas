# stornas

A NAS/SAN appliance for homelabs and small KubeVirt clusters.
Successor in spirit to FreeNAS, built on the Red Hat ecosystem:
CentOS Stream 10, bootc, mdadm, LVM, DRBD 9, LINSTOR, XFS, LIO, MicroShift.

stornas ships as a single bootc image layered on
[epheo/microshift](https://github.com/epheo/microshift).
It boots, serves storage, and upgrades with zero external connectivity.

Every feature below is exercised end to end in CI:
a real browser drives the UI against booted appliance VMs,
real NFS, SMB, and iSCSI clients drive the data path,
and real failures (disk pulls, node kills, network partitions) drive recovery.
Nothing is documented here before its flow is green.

## Features

**Pools**
- Node-local storage pools from whole disks, addressed by stable by-id paths.
- raid1 mirroring in md below the LVM PV; disk inventory with SMART
  verdict, temperature, and power-on hours per disk.
- A pulled disk degrades the pool, raises an alert, names the dead member,
  and the UI replace flow rebuilds onto a spare while IO continues.
- Pool deletion dismantles the host: volumes guard it, then the VG, the md
  array, and member signatures are wiped and the disks read unclaimed again.

**Volumes**
- Every volume is a Kubernetes PVC; SAN LUNs, NAS shares, and VM disks
  share one CSI path (LINSTOR CSI).
- Two classes: `stornas-local` (LVM only, zero overhead, default) and
  `stornas-replicated` (DRBD 9, two synchronous replicas, protocol C).
  Replication is a StorageClass choice, never an install mode.
- Filesystem (XFS) or raw block mode, chosen at creation.
- Online resize, snapshots, and restore-to-new-volume from the UI.
  Restores inherit the source volume's class and mode.
- Volumes bind at creation with no consumer: the operator places podless
  claims (UI volumes, imports) and pins restores to their snapshot's
  node; a claim a pod references keeps scheduler placement.

**SAN (iSCSI)**
- Targets with per-initiator CHAP; credentials live in Secrets and
  rotate without recreating the target.
- Replicated LUNs carry a VIP; on node loss the target re-places to the
  peer, the VIP moves with gratuitous ARP, and initiators reconnect.
  Active/passive in v1.

**NAS (NFS and SMB)**
- NFSv4 only; shares mount as `server:/<namespace>-<name>` under an
  fsid=0 pseudo root (the composefs rootfs cannot anchor the v4 tree).
- The client list is live access control: narrowing it locks clients out
  immediately, without a re-export cycle.
- SMB uses the shared-folder model: valid users gate access, and inside
  the share every user acts as one owner (`force user = root`), matching
  NFS's `no_root_squash` default.
- One user object feeds both UI login and the samba passdb; deleting the
  user revokes both.

**Failover and recovery**
- Node death: replicated volumes keep serving from the peer, exports and
  VIPs move, consumers reattach on the survivor with their data
  (the Kubernetes non-graceful shutdown flow), and the returned node is
  fenced clean and resynced.
- Two-node split brain: IO suspends instead of diverging silently
  (`auto-quorum: suspend-io`), and the UI owns the pick-survivor flow.
- Master death: worker data planes keep serving; placement stays put when
  the master returns.
- LINSTOR controller down: IO continues; provisioning blocks and recovers.

**Appliance lifecycle**
- First boot generates an admin password (console and MOTD) and the UI
  nudges a change; admin and viewer roles.
- Upgrades arrive as a new bootc image, air-gap friendly
  (`bootc switch --transport oci-archive`); greenboot gates the boot and
  a bad image rolls back automatically with data intact.
- No registry pull, package install, or download happens at runtime:
  stornas, Piraeus/LINSTOR, and CSI images are embedded and digest-pinned.

## Architecture

Three moving parts, one rule each:

- **stornas-operator** makes every decision. It reconciles the CRDs,
  registers pools with LINSTOR, places targets and shares on the DRBD
  primary, and drives failover and deletion finalizer chains.
- **stornas-agent** is a dumb, privileged DaemonSet, the only thing that
  touches the host. It reconciles md/LVM, LIO configfs, samba, and
  kernel nfsd from CRD specs and reports inventory; it never decides.
- **stornas server** serves the API and the SvelteKit UI, streams live
  state over a websocket, and writes CRDs on behalf of session users.

Piraeus operator, LINSTOR controller (master) and satellites (per node),
and LINSTOR CSI do volume orchestration; stornas never drives DRBD directly.

Raid lives in mdadm below the LVM PV: each layer has one owner, and
LINSTOR only ever sees a linear pool on a plain device, the one
configuration its containerized satellite can always activate.

stornas is the storage plane only; VM features belong to dotvirt.

## CRDs (storage.stornas.io/v1alpha1)

```yaml
kind: StoragePool                 # pools are node local
spec:
  node: node-a
  devices: [/dev/disk/by-id/wwn-0x5000c500a1b2c3d4, ...]
  raid: raid1                     # none | raid1
```

```yaml
kind: Target                      # iSCSI
spec:
  vip: 192.168.1.50/24            # required when any LUN is replicated
  luns:
    - id: 0
      claimName: vm-disk-0        # PVC, block mode
  initiators:
    - iqn: iqn.1994-05.com.redhat:client1
      chapSecretRef: client1-chap
```

```yaml
kind: Share                       # NFS / SMB
spec:
  claimName: media                # PVC, filesystem mode, XFS
  nfs:
    clients: ["192.168.1.0/24(rw,no_root_squash)"]
  smb:
    name: media
    validUsers: [alice]
```

```yaml
kind: LocalUser                   # UI login + samba passdb
spec:
  role: admin                     # admin | viewer
  smb: true
  passwordSecretRef: alice-password
```

## Failure matrix

| Failure | Behavior | Action |
|---|---|---|
| Disk dies in raid1 pool | Pool Degraded, IO continues, alert raised | Replace flow in UI |
| Node dies, replicated volume | Peer keeps IO; VIP moves; consumers reattach | Auto resync on return |
| Node dies, local volume | Volume unavailable until node returns | Stated plainly in UI |
| Master dies, workers alive | Data IO continues; no provisioning; UI down | Restore master |
| 2-node split brain | IO suspended on both sides | Pick-survivor flow in UI |
| Bad upgrade image | greenboot fails; bootc rolls back | None; data intact |
| LINSTOR controller down | IO fine; provisioning blocked | Recovers with the controller |

Every row is enforced by a CI gate.
Two-node clusters cannot have quorum; the documented upgrade is a third
machine running only a diskless tiebreaker satellite.

## Building and testing

```
make ci                # generate, build, lint, unit tests, web build
make images            # server/agent/operator images (local podman)
make image             # the full bootc appliance image
make vm-test           # boot gate: single node, full UI/UX flows, air gap
make replication-test  # two nodes: replication, failover, clients, split brain
make upgrade-test      # air-gapped upgrade and poisoned-image rollback
```

The gates boot real VMs with qemu and drive the UI with playwright.
The rule they enforce: a feature exists once its UI/UX flow passes e2e;
kernel, mdadm, and DRBD internals are never the test subject, only
stornas behavior and configuration.

## Not yet

Volume cloning, raid5/raid10, async remote replication (DRBD protocol A),
ALUA multipath, scheduled snapshots, quotas, OIDC, per-user SMB
ownership and ACLs, NVMe/TCP, controller HA.

## Not planned

Fibre Channel, FCoE, NVMe/RDMA, NVMe/FC. Each needs an HBA, a CNA, or a
lossless DCB fabric, none of which qemu can present, so their flows can
never pass a gate and the rule above bars them from existing.

Non-disruptive SAN failover, under any transport. A Secondary DRBD device
is unreadable, so the standby node cannot export a path before promotion;
active/passive is bounded by DRBD, not by iSCSI. NVMe/TCP above is a
performance item, and ALUA multipath is per-node link redundancy.

## Layout

```
cmd/stornas/         API + UI server
cmd/stornas-agent/   privileged node DaemonSet, only thing touching the host
internal/            server and agent internals
web/                 SvelteKit UI
operator/            CRDs + controllers (StoragePool, Target, Share, LocalUser)
image/               bootc layer, DRBD 9 kmod build, cluster manifests
hack/                e2e gates
```

## License

Apache License 2.0, see [LICENSE.md](LICENSE.md).
