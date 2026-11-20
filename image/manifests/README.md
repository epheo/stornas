# Cluster manifests

Baked into the bootc image and applied from disk by MicroShift:

- `stornas/` -> `/usr/lib/microshift/manifests.d/010-stornas/`
  stornas CRDs (synced by `make sync-manifests`), StorageClasses,
  VolumeSnapshotClass, agent, operator, server.

The distro's `000-r-drbd` applies first: r-drbd's CRDs, CSIDriver,
controller and agent. Its images, the CSI sidecars, and the DRBD module
come with the distro; only the stornas images are embedded here.

Each StoragePool gets an r-drbd Pool of the same name (VG
`stornas-<pool>`, thin pool `thin`); StorageClasses select by the
`tier` parameter, so they stay node agnostic.
Pools keep r-drbd's capacity defaults: no thin overcommit, 5% reserve.
A pool name's length plus its dashes stays within 21: a snapshot's
device-mapper name carries the VG name, where each dash counts twice.

The external snapshot-controller ships with MicroShift, not here;
these manifests add only the VolumeSnapshotClass and the
csi-snapshotter sidecar in the r-drbd controller pod.
