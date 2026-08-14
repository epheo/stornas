#!/usr/bin/env bash
# Boot acceptance test, same harness shape as the microshift distro's
# scripts/vm-test.sh: bootc-image-builder turns the bootc image into a
# qcow2 with a root ssh key baked in, plain qemu-system boots it with
# scratch disks, ssh rides a user-net hostfwd, and the console log is the
# failure artifact. On top of the distro's boot checks this validates the
# storage stack: drbd kmod, piraeus + LINSTOR, a raid1 StoragePool (md
# below the PV, DESIGN.md) backing LINSTOR CSI, a PVC and snapshot
# through it, and the UI in a real browser. The disk failure matrix row
# runs on the same pool: a member hot-unplugged over QMP must show
# Degraded in the UI and the dialog-driven replace must rebuild onto the
# spare. The assertions stay on stornas's own surface (status, UI,
# replace flow); raid IO semantics belong to the kernel.
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
# Needs a root-capable podman (PODMAN='sudo podman' in CI),
# qemu-system-x86_64, and playwright's chromium for the UI phases
# (cd web && npx playwright install chromium). KVM when present.
set -euo pipefail
# shellcheck source=hack/lib.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib.sh"

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
	log "DIAGNOSTICS: pool and LVM state"
	kc get storagepool test -o yaml 2>&1 | sed -n '/status:/,$p' | tail -30 || true
	vssh sh -c 'pvs; lvs -a -o name,attr,sync_percent,devices' 2>&1 || true
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

# ssh adds a remote shell evaluation layer that strips quoting (the
# parens in a kubectl jsonpath break the remote shell) — re-quote every
# arg with printf %q so commands run remotely exactly as written here.
vssh() {
	ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o LogLevel=ERROR -i "$WORKDIR/id" -p "$SSH_PORT" \
		root@127.0.0.1 "$(printf '%q ' "$@")"
}
kc() { vssh kubectl --kubeconfig /var/lib/microshift/resources/kubeadmin/kubeconfig "$@"; }

# One QMP command per connection; python3 because the runner has no socat.
# Exits nonzero on a QMP error reply: a refused device_del would silently
# void every assertion built on it.
qmp() { # qmp <json-execute-line>
	python3 - "$WORKDIR/qmp.sock" "$1" <<'PY'
import json, socket, sys
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
f = s.makefile("rw")
f.readline()
f.write(json.dumps({"execute": "qmp_capabilities"}) + "\n")
f.flush()
f.readline()
f.write(sys.argv[2] + "\n")
f.flush()
resp = f.readline()
print(resp)
sys.exit(1 if '"error"' in resp else 0)
PY
}

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
sync_rootful_image "$IMAGE" "$PODMAN"
# Registry weather must not burn a run this long; the pull retries.
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

log "booting the appliance with raid members and a spare"
for d in scratch raidb spare; do
	truncate -s 10G "$WORKDIR/$d.raw"
done
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
	-device virtio-blk-pci,drive=scratch,serial=STORNASTEST,id=disk-a \
	-device pcie-root-port,id=hotplug-port,slot=9 \
	-drive "file=$WORKDIR/raidb.raw,if=none,format=raw,id=raidb" \
	-device virtio-blk-pci,drive=raidb,serial=STORNASB,id=disk-b,bus=hotplug-port \
	-drive "file=$WORKDIR/spare.raw,if=none,format=raw,id=spare" \
	-device virtio-blk-pci,drive=spare,serial=STORNASC,id=disk-c \
	-qmp "unix:$WORKDIR/qmp.sock,server=on,wait=off" \
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

log "UI answers and login works"
retry 300 "healthz via hostfwd" curl -fsS -m 15 "http://127.0.0.1:$UI_PORT/healthz"
ADMIN_PW=$(kc -n stornas-system get secret admin-password -o jsonpath='{.data.password}' | base64 -d)
curl -fsS -m 15 -c "$WORKDIR/cookies" -H 'Content-Type: application/json' \
	-d "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}" \
	"http://127.0.0.1:$UI_PORT/api/v1/login" >/dev/null || die "login failed"
curl -fsS -m 15 -b "$WORKDIR/cookies" "http://127.0.0.1:$UI_PORT/api/v1/state" | grep -q '"pools":' || die "state missing pools"

# Real-browser phases; need playwright's chromium
# (npx playwright install chromium, once per machine).
ui_phase() { # ui_phase <phase> [user] [password]
	UI_URL="http://127.0.0.1:$UI_PORT" UI_USER="${2:-admin}" ADMIN_PW="${3:-$ADMIN_PW}" \
		node web/e2e/ui.mjs "$1" || die "UI phase $1 failed"
}

log "creating the raid1 pool through the UI form"
ui_phase create-pool
retry 600 "storage pool Available" pool_available

log "creating the volume through the UI form"
TARGET_VOL=boot-test ui_phase create-volume
# WaitForFirstConsumer: the consumer pod is scaffolding, not the feature.
kc apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: boot-test-consumer
  namespace: stornas-system
spec:
  restartPolicy: Never
  # The agent SA's privileged SCC lets the consumer write the volume as
  # root; restricted-v2 leaves the mount root-owned (no fsGroup applied).
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "3600"]
      securityContext:
        runAsUser: 0
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: boot-test
EOF
retry 600 "PVC Bound" pvc_bound

# The session layer is the security boundary; every refusal is product
# behavior worth pinning.
log "auth boundaries: anonymous and wrong-password refused"
http_code() { curl -s -m 15 -o /dev/null -w '%{http_code}' "$@"; }
code=$(http_code "http://127.0.0.1:$UI_PORT/api/v1/state")
[ "$code" = 401 ] || die "anonymous state got $code, want 401"
code=$(http_code -H 'Content-Type: application/json' \
	-d '{"username":"admin","password":"not-the-password"}' \
	"http://127.0.0.1:$UI_PORT/api/v1/login")
[ "$code" = 401 ] || die "wrong-password login got $code, want 401"

log "viewer role: created in the UI, read allowed, mutations refused"
TARGET_USER=eyes TARGET_USER_PW=e2eviewer123 ui_phase create-user
viewer_login() {
	curl -fsS -m 15 -c "$WORKDIR/vcookies" -H 'Content-Type: application/json' \
		-d '{"username":"eyes","password":"e2eviewer123"}' \
		"http://127.0.0.1:$UI_PORT/api/v1/login" >/dev/null
}
retry 60 "viewer login" viewer_login
curl -fsS -m 15 -b "$WORKDIR/vcookies" "http://127.0.0.1:$UI_PORT/api/v1/state" | grep -q '"pools":' \
	|| die "viewer cannot read state"
code=$(http_code -b "$WORKDIR/vcookies" -H 'Content-Type: application/json' \
	-d '{"name":"nope","size":"1Gi","storageClass":"stornas-local"}' \
	"http://127.0.0.1:$UI_PORT/api/v1/volumes")
[ "$code" = 403 ] || die "viewer mutation got $code, want 403"
curl -fsS -m 15 -b "$WORKDIR/vcookies" -X POST "http://127.0.0.1:$UI_PORT/api/v1/logout" >/dev/null \
	|| die "logout failed"
code=$(http_code -b "$WORKDIR/vcookies" "http://127.0.0.1:$UI_PORT/api/v1/state")
[ "$code" = 401 ] || die "logged-out session still valid (got $code)"

# Sessions are in-memory by design: a server restart must refuse old
# cookies (fail closed) and serve fresh logins immediately.
log "server restart: UI back, stale session refused"
kc -n stornas-system rollout restart deploy/stornas-server
kc -n stornas-system rollout status deploy/stornas-server --timeout=180s
retry 120 "healthz after server restart" curl -fsS -m 15 "http://127.0.0.1:$UI_PORT/healthz"
stale_refused() { [ "$(http_code -b "$WORKDIR/cookies" "http://127.0.0.1:$UI_PORT/api/v1/state")" = 401 ]; }
retry 60 "stale session refused" stale_refused
relogin() {
	curl -fsS -m 15 -c "$WORKDIR/cookies" -H 'Content-Type: application/json' \
		-d "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}" \
		"http://127.0.0.1:$UI_PORT/api/v1/login" >/dev/null
}
retry 60 "fresh login after restart" relogin

log "UI renders live data in a real browser"
ui_phase smoke
log "viewer chrome hides management affordances"
ui_phase viewer eyes e2eviewer123
log "user delete revokes the login"
TARGET_USER=eyes ui_phase delete-user
eyes_dead() {
	[ "$(http_code -H 'Content-Type: application/json' \
		-d '{"username":"eyes","password":"e2eviewer123"}' \
		"http://127.0.0.1:$UI_PORT/api/v1/login")" = 401 ]
}
retry 60 "deleted user cannot log in" eyes_dead

log "snapshot and restore through the UI dialogs"
TARGET_VOL=boot-test TARGET_SNAP=boot-snap ui_phase snapshot-volume
snap_ready() { kc -n stornas-system get volumesnapshot boot-snap -o jsonpath='{.status.readyToUse}' | grep -q true; }
retry 300 "snapshot ready" snap_ready
TARGET_SNAP=boot-snap TARGET_VOL=boot-restore ui_phase restore-snapshot
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

log "resize through the UI dialog grows the mounted volume"
TARGET_VOL=boot-test TARGET_SIZE=2Gi ui_phase resize-volume
resized() { kc -n stornas-system get pvc boot-test -o jsonpath='{.status.capacity.storage}' | grep -qx 2Gi; }
retry 300 "PVC capacity grew to 2Gi" resized
kc -n stornas-system exec boot-test-consumer -- sh -c 'df -k /data | tail -1' \
	| awk '{exit ($2 > 1900000) ? 0 : 1}' || die "filesystem did not grow with the volume"

log "pulling a raid1 member: status must degrade and name the victim"
qmp '{"execute": "device_del", "arguments": {"id": "disk-b"}}' || die "device_del refused"
kc -n stornas-system exec boot-test-consumer -- sh -c 'echo during-pull > /data/marker && sync' \
	|| die "PVC IO blocked after the disk pull"
pool_health() { kc get storagepool test -o jsonpath='{.status.health}' | grep -qx "$1"; }
device_state() { kc get storagepool test -o jsonpath="{.status.devices[?(@.path=='$1')].state}" | grep -qx "$2"; }
pool_degraded() { pool_health Degraded; }
retry 300 "raid pool Degraded" pool_degraded
dead_named() { device_state /dev/disk/by-id/virtio-STORNASB Missing; }
retry 120 "dead member named by its spec path" dead_named

log "replace flow through the UI: pick the spare in the dialog"
ui_phase degraded-replace
pool_online() { pool_health Online; }
retry 600 "raid pool back Online after rebuild" pool_online
spare_insync() { device_state /dev/disk/by-id/virtio-STORNASC InSync; }
retry 120 "spare InSync" spare_insync
kc get storagepool test -o jsonpath='{.status.devices[*].state}' | grep -q Missing \
	&& die "dead member still reported after replace"
kc -n stornas-system exec boot-test-consumer -- cat /data/marker | grep -q during-pull \
	|| die "marker written on the degraded pool is missing"
ui_phase online

log "delete flows through the UI: snapshot, then the restored volume"
TARGET_SNAP=boot-snap ui_phase delete-snapshot
snap_gone() { ! kc -n stornas-system get volumesnapshot boot-snap >/dev/null 2>&1; }
retry 120 "snapshot gone" snap_gone
# pvc-protection holds a claimed volume; the consumer goes first.
kc -n stornas-system delete pod boot-restore-consumer --wait
TARGET_VOL=boot-restore ui_phase delete-volume
restore_gone() { ! kc -n stornas-system get pvc boot-restore >/dev/null 2>&1; }
retry 180 "restored volume gone" restore_gone

log "pool delete through the UI dismantles the host state"
kc -n stornas-system delete pod boot-test-consumer --wait
TARGET_VOL=boot-test ui_phase delete-volume
# PV gone means the CSI finished dropping the LINSTOR resource; only
# then does the pool's delete guard open.
pvs_gone() { [ "$(kc get pv --no-headers 2>/dev/null | wc -l)" = 0 ]; }
retry 300 "all PVs gone" pvs_gone
ui_phase delete-pool
pool_gone() { ! kc get storagepool test >/dev/null 2>&1; }
retry 300 "pool CR gone (host wipe confirmed)" pool_gone
vssh sh -c 'vgs stornas-test 2>/dev/null' && die "VG survived the pool delete"
vssh test -e /dev/md/stornas-test && die "md array survived the pool delete"
vssh sh -c 'mdadm --examine /dev/disk/by-id/virtio-STORNASTEST 2>/dev/null' \
	&& die "member superblock survived the pool delete"

# Last: it retires the generated password every step above logs in with.
log "first-boot onboarding: nudge, password change, relogin"
NEW_PW=stornas-e2e-pw1 ui_phase onboarding
code=$(http_code -H 'Content-Type: application/json' \
	-d "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}" \
	"http://127.0.0.1:$UI_PORT/api/v1/login")
[ "$code" = 401 ] || die "retired generated password still logs in (got $code)"

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
