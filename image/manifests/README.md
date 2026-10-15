# Cluster manifests

Baked into the bootc image and applied from disk by MicroShift, in
lexical order:

- `r-drbd/` -> `/usr/lib/microshift/manifests.d/000-r-drbd/`
  the r-drbd storage plane: namespace r-drbd-system, RBAC, the Pool,
  Volume and Snapshot CRDs (`r-drbd crds` output), CSIDriver r-drbd.io,
  the controller Deployment and the agent DaemonSet. Copied from
  r-drbd's deploy/base; bump with the image tag in
  image/embedded-images.txt.
- `stornas/` -> `/usr/lib/microshift/manifests.d/010-stornas/`
  stornas CRDs (synced by `make sync-manifests`), StorageClasses,
  VolumeSnapshotClass, agent, operator, server.

Every referenced workload image is embedded as an OCI archive under
`/usr/lib/embedded-images/` (list: image/embedded-images.txt). Nothing
is fetched at runtime.

Each StoragePool gets an r-drbd Pool of the same name (VG
`stornas-<pool>`, thin pool `thin`); StorageClasses select by the
`tier` parameter, so they stay node agnostic.

The external snapshot-controller ships with MicroShift, not here;
these manifests add only the VolumeSnapshotClass and the
csi-snapshotter sidecar in the r-drbd controller pod.
