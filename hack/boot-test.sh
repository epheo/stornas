#!/usr/bin/env bash
# Boot acceptance test, same harness shape as the microshift distro's
# scripts/vm-test.sh: bootc-image-builder turns the bootc image into a
# qcow2 with a root ssh key baked in, plain qemu-system boots it with a
# scratch disk, ssh rides a user-net hostfwd, and the console log is the
# failure artifact. On top of the distro's boot checks this validates the
# storage stack: drbd kmod, piraeus + LINSTOR, a StoragePool converging on
# the scratch disk, a PVC binding through LINSTOR CSI, and the UI.
#
# The guest boots with restrict=on by default: no outbound at all, only
# the hostfwd ports in (DESIGN.md air gap; the distro embeds the full
# MicroShift payload since 2026-08). AIRGAP=0 reopens outbound for
# debugging. The pull assert stays unconditional: no stornas, piraeus,
# or sig-storage image may be fetched at runtime.
# ipv6=off matches the distro's vm-test: slirp's fec0:: RA can land
# before the DHCPv4 lease and MicroShift then picks IPv6 single-stack,
# where OVN-K never reaches node readiness.
#
# Needs a root-capable podman (PODMAN='sudo podman' in CI) and
# qemu-system-x86_64. KVM is used when present, TCG otherwise.
set -euo pipefail

IMAGE=${IMAGE:-localhost/stornas-os:dev}
PODMAN=${PODMAN:-podman}
BIB_IMAGE=${BIB_IMAGE:-quay.io/centos-bootc/bootc-image-builder:latest}
WORKDIR=${WORKDIR:-$(mktemp -d /tmp/stornas-vm.XXXXXX)}
SSH_PORT=${SSH_PORT:-2222}
UI_PORT=${UI_PORT:-8080}
VM_MEM=${VM_MEM:-8192}
KEEP=${KEEP:-0}
AIRGAP=${AIRGAP:-1}
QEMU_PID=""

log() { printf '\n== %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

diagnostics() {
	log "DIAGNOSTICS: pods"
	kc get pods -A -o wide 2>&1 || true
	log "DIAGNOSTICS: not-running pod describes"
	for p in $(kc get pods -A --no-headers 2>/dev/null \
			| awk '$4 !~ /Running|Completed/ {print $1"/"$2}'); do
		kc -n "${p%/*}" describe pod "${p#*/}" 2>&1 | tail -15 || true
	done
	log "DIAGNOSTICS: linstorcluster status"
	kc get linstorcluster -o yaml 2>&1 | sed -n '/status:/,$p' | tail -25 || true
	log "DIAGNOSTICS: piraeus operator log tail"
	kc -n piraeus-datastore logs deploy/piraeus-operator-controller-manager --tail=25 2>&1 || true
	log "DIAGNOSTICS: snapshot path"
	kc get volumesnapshot,volumesnapshotcontent -A 2>&1 || true
	kc -n stornas-system describe volumesnapshot boot-snap 2>&1 | sed -n '/Status:/,$p' | tail -12 || true
	kc -n piraeus-datastore logs deploy/linstor-csi-controller -c csi-snapshotter --tail=20 2>&1 | tail -15 || true
	kc -n piraeus-datastore exec deploy/linstor-controller -- linstor snapshot list 2>&1 || true
	kc -n piraeus-datastore exec deploy/linstor-controller -- linstor err list 2>&1 | tail -15 || true
	log "DIAGNOSTICS: import-embedded-images"
	vssh journalctl -u import-embedded-images --no-pager 2>&1 | tail -10 || true
	log "DIAGNOSTICS: crio image pulls"
	vssh journalctl -u crio --no-pager 2>/dev/null | grep 'Pulling image' | tail -15 || true
}

cleanup() {
	rc=$?
	if [ $rc -ne 0 ]; then
		log "FAILED (rc=$rc)"
		# ssh may itself be the failure; diagnostics degrade to noise then.
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

retry() { # retry <seconds> <description> <cmd...>; cmd runs in this shell
	local deadline=$(( $(date +%s) + $1 )) desc=$2
	printf 'waiting up to %ss for: %s\n' "$1" "$desc"
	shift 2
	until "$@" >/dev/null 2>&1; do
		if [ "$(date +%s)" -gt "$deadline" ]; then
			die "timed out waiting for: $desc"
		fi
		sleep 5
	done
	log "ok: $desc"
}

# ssh adds a remote shell evaluation layer that strips quoting (the
# parens in a kubectl jsonpath break the remote shell) — re-quote every
# arg with printf %q so commands run remotely exactly as written here.
vssh() {
	ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o LogLevel=ERROR -i "$WORKDIR/id" -p "$SSH_PORT" \
		root@127.0.0.1 "$(printf '%q ' "$@")"
}
kc() { vssh kubectl --kubeconfig /var/lib/microshift/resources/kubeadmin/kubeconfig "$@"; }

node_ready() { kc get nodes --no-headers | grep -q ' Ready'; }
piraeus_up() { kc -n piraeus-datastore get deploy piraeus-operator-controller-manager -o jsonpath='{.status.availableReplicas}' | grep -q 1; }
controller_up() { kc -n piraeus-datastore get pods -l app.kubernetes.io/component=linstor-controller --no-headers | grep -q Running; }
satellite_up() { kc -n piraeus-datastore get pods -l app.kubernetes.io/component=linstor-satellite --no-headers | grep -q Running; }
stornas_up() { [ "$(kc -n stornas-system get pods --no-headers | grep -c Running)" -ge 3 ]; }
pool_available() { kc get storagepool test -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' | grep -q True; }
pvc_bound() { kc -n stornas-system get pvc boot-test -o jsonpath='{.status.phase}' | grep -q Bound; }

mkdir -p "$WORKDIR/output"

log "building qcow2 from $IMAGE"
ssh-keygen -t ed25519 -N '' -f "$WORKDIR/id" -q
cat > "$WORKDIR/config.toml" <<EOF
[[customizations.user]]
name = "root"
key = "$(cat "$WORKDIR/id.pub")"

# The image plus its imported embedded images plus the distro's pulled
# release images overflow bib's default root; 8.4G filled to 100%.
[[customizations.filesystem]]
mountpoint = "/"
minsize = "30 GiB"
EOF
# Sync the image into rootful storage by ID, not mere existence: a stale
# rootful copy would boot silently and old bugs would resurface.
if [ "$PODMAN" != podman ] && podman image exists "$IMAGE"; then
	want=$(podman image inspect -f '{{.Id}}' "$IMAGE")
	have=$($PODMAN image inspect -f '{{.Id}}' "$IMAGE" 2>/dev/null || true)
	if [ "$want" != "$have" ]; then
		log "syncing $IMAGE into rootful podman storage"
		podman save "$IMAGE" | $PODMAN load
	fi
else
	$PODMAN image exists "$IMAGE" || die "image $IMAGE not found in $PODMAN storage"
fi
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

log "booting the appliance with a scratch disk"
truncate -s 20G "$WORKDIR/scratch.raw"
ACCEL=tcg
[ -w /dev/kvm ] && ACCEL=kvm
# bootc disks are UEFI; SeaBIOS sits at a blank screen with no serial
# output and the guest never faults memory in. Find OVMF or refuse.
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
	-netdev "user,id=n0,ipv6=off$([ "$AIRGAP" = 1 ] && echo ,restrict=on),hostfwd=tcp::${SSH_PORT}-:22,hostfwd=tcp::${UI_PORT}-:30080" \
	-device virtio-net-pci,netdev=n0 \
	-device virtio-rng-pci \
	-serial "file:$WORKDIR/console.log" \
	-display none -daemonize -pidfile "$WORKDIR/qemu.pid"
QEMU_PID=$(cat "$WORKDIR/qemu.pid")
log "VM running (qemu pid $QEMU_PID, no libvirt); console: $WORKDIR/console.log"

retry 600 "ssh reachable" vssh true
retry 600 "microshift service active" vssh systemctl is-active microshift
retry 600 "node Ready" node_ready

log "drbd kmod loads on the image kernel"
vssh modprobe drbd
vssh modinfo -F version drbd

log "waiting for the storage stack"
retry 900 "piraeus operator available" piraeus_up
retry 900 "linstor controller up" controller_up
retry 900 "linstor satellite up" satellite_up
retry 600 "stornas operator, agent, server running" stornas_up

log "creating a StoragePool on the scratch disk"
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

log "provisioning a PVC through LINSTOR CSI"
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: boot-test
  namespace: stornas-system
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: stornas-local
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: boot-test-consumer
  namespace: stornas-system
spec:
  restartPolicy: Never
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "3600"]
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: boot-test
EOF
retry 600 "PVC Bound" pvc_bound

log "UI answers and login works"
retry 300 "healthz via hostfwd" curl -fsS "http://127.0.0.1:$UI_PORT/healthz"
ADMIN_PW=$(kc -n stornas-system get secret admin-password -o jsonpath='{.data.password}' | base64 -d)
curl -fsS -c "$WORKDIR/cookies" -H 'Content-Type: application/json' \
	-d "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}" \
	"http://127.0.0.1:$UI_PORT/api/v1/login" >/dev/null || die "login failed"
curl -fsS -b "$WORKDIR/cookies" "http://127.0.0.1:$UI_PORT/api/v1/state" | grep -q '"pools":' || die "state missing pools"

log "snapshot and restore through the appliance API"
curl -fsS -b "$WORKDIR/cookies" -H 'Content-Type: application/json' \
	-d '{"name":"boot-snap","volume":"boot-test"}' \
	"http://127.0.0.1:$UI_PORT/api/v1/snapshots" >/dev/null || die "snapshot create failed"
snap_ready() { kc -n stornas-system get volumesnapshot boot-snap -o jsonpath='{.status.readyToUse}' | grep -q true; }
retry 300 "snapshot ready" snap_ready
curl -fsS -b "$WORKDIR/cookies" -H 'Content-Type: application/json' \
	-d '{"name":"boot-restore","fromSnapshot":"boot-snap"}' \
	"http://127.0.0.1:$UI_PORT/api/v1/volumes" >/dev/null || die "restore create failed"
# WaitForFirstConsumer: the restored PVC binds only under a consumer.
kc apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: boot-restore-consumer
  namespace: stornas-system
spec:
  restartPolicy: Never
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "3600"]
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: boot-restore
EOF
restore_bound() { kc -n stornas-system get pvc boot-restore -o jsonpath='{.status.phase}' | grep -q Bound; }
retry 300 "restored volume Bound" restore_bound

# The air-gap contract: every ref in the embedded manifest must come
# from the store, never a registry. The drbd9-* loader is deliberately
# outside the set: piraeus renders it once before the satellite patch
# can exist (its webhook gates the config CR), the patched DaemonSet
# replaces that pod seconds later, and the appliance never needs it.
log "no embedded image was pulled at runtime"
pull_lines=$(vssh journalctl -u crio --no-pager 2>/dev/null | grep 'Pulling image' || true)
pulled=""
while read -r _ ref; do
	[ -z "$ref" ] && continue
	hit=$(grep -F "$ref" <<<"$pull_lines" || true)
	[ -n "$hit" ] && pulled="$pulled$hit"$'\n'
done <<<"$(vssh cat /usr/lib/embedded-images/manifest)"
[ -z "$pulled" ] || { printf '%s\n' "$pulled"; die "embedded images were pulled from a registry"; }

log "greenboot reports healthy"
vssh journalctl -b -u greenboot-healthcheck 2>/dev/null | grep -qiE 'GREEN|health-check passed' \
	|| log "warn: no greenboot verdict found (unit may be named differently)"

log "BOOT TEST PASSED"
