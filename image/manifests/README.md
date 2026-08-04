# Cluster manifests

Baked into the bootc image and applied from disk by MicroShift, in
lexical order:

- `piraeus/` -> `/usr/lib/microshift/manifests.d/000-piraeus/`
  piraeus-operator v2.11.0, vendored via kustomize render; regenerate
  with a new ref and bump image/embedded-images.txt together.
- `stornas/` -> `/usr/lib/microshift/manifests.d/010-stornas/`
  stornas CRDs (synced by `make sync-manifests`), LinstorCluster, the
  satellite patch that drops the module loader (kmod ships in the OS),
  StorageClasses, agent, operator, server.

Every referenced workload image is embedded as an OCI archive under
`/usr/lib/embedded-images/` (list: image/embedded-images.txt); pods use
pull policy Never. Nothing is fetched at runtime.

The LINSTOR storage pool name is `stornas` by convention: every
StoragePool CR registers its VG under that name so StorageClasses stay
node agnostic.

Not yet included: the external snapshot-controller (comes with the
snapshots milestone; the VolumeSnapshotClass is inert until then).
