# Cluster manifests

Applied on top of the microshift distro, in order:

1. piraeus-operator v2 (upstream kustomize or helm; pin the version in the
   distro image, not here).
2. This kustomization: LinstorCluster, StorageClasses, VolumeSnapshotClass.
3. stornas operator and server (operator/config, not yet wired here).

The LINSTOR storage pool name is `stornas` by convention: every StoragePool
CR registers its VG under that name so StorageClasses stay node agnostic.
