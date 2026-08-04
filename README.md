# stornas

A NAS/SAN appliance for homelabs and small KubeVirt clusters.
Successor in spirit to FreeNAS, built on the Red Hat ecosystem:
CentOS Stream 10, bootc, LVM, DRBD 9, LINSTOR, XFS, LIO, MicroShift.

- Single node by default; replication is a StorageClass, not an install mode.
- Every volume is a PVC: SAN LUNs, NAS shares, and VM disks share one CSI path.
- Block volumes replicate across hosts with DRBD (protocol C, optional A).
- Simple web UI focused on pools, shares, targets, and replication health.
- Ships as a bootc image layered on [epheo/microshift](https://github.com/epheo/microshift).

See [DESIGN.md](DESIGN.md) for architecture, CRDs, and the failure matrix.

## Layout

```
cmd/stornas/         API + UI server
cmd/stornas-agent/   privileged node DaemonSet, only thing touching the host
internal/            server and agent internals
web/                 SvelteKit UI
operator/            CRDs + controllers (StoragePool, Target, Share, LocalUser)
image/               bootc layer, DRBD 9 kmod build, cluster manifests
```

## License

Apache License 2.0, see [LICENSE.md](LICENSE.md).
