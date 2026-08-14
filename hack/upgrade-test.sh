#!/usr/bin/env bash
# Upgrade and rollback acceptance: the appliance lifecycle promise.
# Boot image A with a pool and data on it, bootc switch to a derived
# good image delivered as an oci-archive over ssh (the air-gapped path:
# no registry anywhere), and assert the stack and the data survive the
# reboot. Then switch to a deliberately poisoned image whose required
# greenboot check fails: greenboot must go red and the boot counter
# must fall the host back to the good deployment, data still intact.
# The kmod-mismatch matrix row collapses to this: a mismatch fails the
# storage check, and the same machinery must recover the appliance.
#
# Needs a root-capable podman (PODMAN='sudo podman' in CI),
# qemu-system-x86_64, and OVMF. KVM when present.
set -euo pipefail
# shellcheck source=hack/lib.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib.sh"

IMAGE=${IMAGE:-localhost/stornas-os:dev}
PODMAN=${PODMAN:-podman}
BIB_IMAGE=${BIB_IMAGE:-quay.io/centos-bootc/bootc-image-builder:latest}
WORKDIR=${WORKDIR:-$(mktemp -d /tmp/stornas-upg.XXXXXX)}
SSH_PORT=${SSH_PORT:-2232}
VM_MEM=${VM_MEM:-8192}
KEEP=${KEEP:-0}
QEMU_PID=""

diagnostics() {
	log "DIAGNOSTICS: bootc status"
	vssh bootc status 2>&1 || true
	log "DIAGNOSTICS: greenboot and redboot journals"
	vssh journalctl -u greenboot-healthcheck --no-pager 2>&1 | tail -30 || true
	vssh journalctl -u redboot-auto-reboot --no-pager 2>&1 | tail -10 || true
	log "DIAGNOSTICS: grub boot counter"
	vssh grub2-editenv list 2>&1 || true
	log "DIAGNOSTICS: pods"
	kc get pods -A -o wide 2>&1 | tail -20 || true
	log "DIAGNOSTICS: pool"
	kc get storagepool test -o yaml 2>&1 | sed -n '/status:/,$p' | tail -20 || true
}

cleanup() {
	rc=$?
	if [ $rc -ne 0 ]; then
		log "FAILED (rc=$rc)"
		diagnostics
		log "last 40 lines of VM console:"
		tail -40 "$WORKDIR/console.log" 2>/dev/null || true
	fi
	if [ "$KEEP" = 1 ]; then
		log "keeping VM (pid $(cat "$WORKDIR/qemu.pid" 2>/dev/null || echo '?')) and $WORKDIR"
		exit $rc
	fi
	[ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
	rm -rf "$WORKDIR"
	exit $rc
}
trap cleanup EXIT

# ssh strips one quoting layer; %q every arg (same as boot-test).
vssh() {
	ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o LogLevel=ERROR -i "$WORKDIR/id" -p "$SSH_PORT" \
		root@127.0.0.1 "$(printf '%q ' "$@")"
}
vscp() { # vscp <local> <remote-path>
	scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o LogLevel=ERROR -i "$WORKDIR/id" -P "$SSH_PORT" "$1" "root@127.0.0.1:$2"
}
kc() { vssh kubectl --kubeconfig /var/lib/microshift/resources/kubeadmin/kubeconfig "$@"; }

node_ready() { kc get nodes --no-headers | grep -q ' Ready'; }
stornas_up() { [ "$(kc -n stornas-system get pods --no-headers | grep -c Running)" -ge 3 ]; }
pool_available() { kc get storagepool test -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' | grep -q True; }
pvc_bound() { kc -n stornas-system get pvc upgrade-test -o jsonpath='{.status.phase}' | grep -q Bound; }
consumer_ready() { kc -n stornas-system get deploy upgrade-consumer -o jsonpath='{.status.availableReplicas}' | grep -q 1; }
data_intact() { kc -n stornas-system exec deploy/upgrade-consumer -- cat /data/marker | grep -q before-upgrade; }
greenboot_green() {
	vssh journalctl -b -u greenboot-healthcheck 2>/dev/null | grep -qiE 'GREEN|health-check passed'
}
ssh_down() { ! vssh true; }

mkdir -p "$WORKDIR/output"

log "building qcow2 from $IMAGE"
ssh-keygen -t ed25519 -N '' -f "$WORKDIR/id" -q
cat > "$WORKDIR/config.toml" <<EOF
[[customizations.user]]
name = "root"
key = "$(cat "$WORKDIR/id.pub")"

# Three image versions transit the ostree repo plus oci archives in
# /var/tmp; the boot-test 30G would run out.
[[customizations.filesystem]]
mountpoint = "/"
minsize = "40 GiB"
EOF
sync_rootful_image "$IMAGE" "$PODMAN"
bib_pull() { $PODMAN pull "$BIB_IMAGE"; }
retry 300 "bootc-image-builder image pulled" bib_pull
$PODMAN run --rm --privileged \
	--security-opt label=type:unconfined_t \
	-v "$WORKDIR/config.toml:/config.toml:ro" \
	-v "$WORKDIR/output:/output" \
	-v /var/lib/containers/storage:/var/lib/containers/storage \
	"$BIB_IMAGE" --type qcow2 --config /config.toml \
	--chown "$(id -u):$(id -g)" "$IMAGE"
DISK="$WORKDIR/output/qcow2/disk.qcow2"
[ -f "$DISK" ] || DISK=$(find "$WORKDIR/output" -name '*.qcow2' | head -1)
[ -n "$DISK" ] || die "bootc-image-builder produced no qcow2"

# The upgrades under test: good adds a marker, bad derives FROM good and
# poisons a required greenboot check, so the fallback deployment after
# the red boots is the good image.
log "building the good and poisoned next images"
cat > "$WORKDIR/Containerfile.good" <<EOF
FROM $IMAGE
RUN touch /usr/lib/stornas-e2e-good
EOF
cat > "$WORKDIR/Containerfile.bad" <<'EOF'
FROM localhost/stornas-e2e:good
RUN printf '#!/bin/bash\necho e2e poisoned image\nexit 1\n' \
		> /etc/greenboot/check/required.d/00_e2e_fail.sh \
	&& chmod +x /etc/greenboot/check/required.d/00_e2e_fail.sh
EOF
$PODMAN build -q -t localhost/stornas-e2e:good -f "$WORKDIR/Containerfile.good" "$WORKDIR"
$PODMAN build -q -t localhost/stornas-e2e:bad -f "$WORKDIR/Containerfile.bad" "$WORKDIR"
# Written by the (possibly rootful) podman with umask 022: readable by
# the scp below without any ownership dance.
$PODMAN save --format oci-archive -o "$WORKDIR/good.tar" localhost/stornas-e2e:good
$PODMAN save --format oci-archive -o "$WORKDIR/bad.tar" localhost/stornas-e2e:bad

log "booting image A"
truncate -s 10G "$WORKDIR/scratch.raw"
ACCEL=tcg
[ -w /dev/kvm ] && ACCEL=kvm
OVMF_CODE=""
for c in /usr/share/edk2/ovmf/OVMF_CODE.fd /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd; do
	[ -f "$c" ] && OVMF_CODE=$c && break
done
[ -n "$OVMF_CODE" ] || die "no OVMF firmware found (install edk2-ovmf / ovmf)"
cp "${OVMF_CODE%CODE*}VARS${OVMF_CODE##*CODE}" "$WORKDIR/ovmf-vars.fd" 2>/dev/null \
	|| cp "$(dirname "$OVMF_CODE")"/OVMF_VARS*.fd "$WORKDIR/ovmf-vars.fd"
qemu-system-x86_64 \
	-machine "q35,accel=$ACCEL" -cpu max -smp "$(nproc)" -m "$VM_MEM" \
	-drive "if=pflash,format=raw,readonly=on,file=$OVMF_CODE" \
	-drive "if=pflash,format=raw,file=$WORKDIR/ovmf-vars.fd" \
	-drive "file=$DISK,if=virtio,format=qcow2" \
	-drive "file=$WORKDIR/scratch.raw,if=none,format=raw,id=scratch" \
	-device virtio-blk-pci,drive=scratch,serial=STORNASTEST \
	-netdev "user,id=n0,ipv6=off,restrict=on,hostfwd=tcp::${SSH_PORT}-:22" \
	-device virtio-net-pci,netdev=n0 \
	-device virtio-rng-pci \
	-serial "file:$WORKDIR/console.log" \
	-display none -daemonize -pidfile "$WORKDIR/qemu.pid"
QEMU_PID=$(cat "$WORKDIR/qemu.pid")
log "VM running (qemu pid $QEMU_PID); console: $WORKDIR/console.log"

retry 600 "ssh reachable" vssh true
retry 600 "node Ready" node_ready
retry 900 "stornas stack running" stornas_up

log "pool, volume, and data that must survive the lifecycle"
NODE=$(kc get nodes --no-headers | awk '{print $1}')
kc apply -f - <<EOF
apiVersion: storage.stornas.io/v1alpha1
kind: StoragePool
metadata:
  name: test
spec:
  node: $NODE
  devices: ["/dev/disk/by-id/virtio-STORNASTEST"]
  raid: none
EOF
retry 600 "storage pool Available" pool_available
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: upgrade-test
  namespace: stornas-system
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: stornas-local
  resources:
    requests:
      storage: 1Gi
---
# A Deployment, not a bare pod: it must come back on its own after
# every reboot this test performs.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: upgrade-consumer
  namespace: stornas-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: upgrade-consumer
  template:
    metadata:
      labels:
        app: upgrade-consumer
    spec:
      serviceAccountName: stornas-agent
      containers:
        - name: c
          image: ghcr.io/epheo/stornas:latest
          imagePullPolicy: Never
          command: [sleep, "infinity"]
          securityContext:
            runAsUser: 0
          volumeMounts:
            - name: v
              mountPath: /data
      volumes:
        - name: v
          persistentVolumeClaim:
            claimName: upgrade-test
EOF
retry 600 "PVC Bound" pvc_bound
retry 300 "consumer ready" consumer_ready
kc -n stornas-system exec deploy/upgrade-consumer -- sh -c 'echo before-upgrade > /data/marker && sync'
retry 300 "greenboot green on A" greenboot_green

log "switching to the good image (oci-archive over ssh, no registry)"
vscp "$WORKDIR/good.tar" /var/tmp/next.tar
vssh bootc switch --transport oci-archive /var/tmp/next.tar
vssh systemctl reboot || true
retry 120 "VM rebooting" ssh_down
retry 600 "ssh back after upgrade" vssh true
booted_good() { vssh test -f /usr/lib/stornas-e2e-good; }
retry 60 "good image booted" booted_good
retry 600 "node Ready after upgrade" node_ready
retry 600 "storage pool Available after upgrade" pool_available
retry 600 "consumer back after upgrade" consumer_ready
retry 120 "data intact after upgrade" data_intact
retry 600 "greenboot green on the good image" greenboot_green
vssh rm -f /var/tmp/next.tar

log "switching to the poisoned image: greenboot must fall back"
vscp "$WORKDIR/bad.tar" /var/tmp/next.tar
vssh bootc switch --transport oci-archive /var/tmp/next.tar
vssh systemctl reboot || true
retry 120 "VM rebooting into the poison" ssh_down
# Red boots cycle on their own; the recovered state is the one where
# the poison check is gone and the good marker is back.
rolled_back() {
	vssh sh -c 'test ! -f /etc/greenboot/check/required.d/00_e2e_fail.sh && test -f /usr/lib/stornas-e2e-good'
}
retry 2400 "rolled back to the good image" rolled_back
retry 600 "node Ready after rollback" node_ready
retry 600 "storage pool Available after rollback" pool_available
retry 600 "consumer back after rollback" consumer_ready
retry 120 "data intact after rollback" data_intact
retry 600 "greenboot green after rollback" greenboot_green
vssh bootc status | grep -qi rollback || true

log "UPGRADE AND ROLLBACK TEST PASSED"
