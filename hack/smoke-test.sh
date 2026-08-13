#!/usr/bin/env bash
# Fast runtime gate, same shape as the distro's scripts/smoke-test.sh:
# boot the OS image as a privileged systemd container and assert the
# storage stack converges. Minutes, no bib, no VM. It found the
# /var/run clobber in under a minute; that class of bug is its job.
#
# What it cannot see is vm-test's job: the drbd kmod (a container runs
# the host kernel, so replication stays untested here), the bootloader,
# greenboot actually gating a boot, and the embedded-image import on a
# cold /var (the container starts with the image's /var).
#
# Requires a root-capable podman: PODMAN='sudo podman' on a real host.
# Not run in CI: microshift-in-container crash-loops on GitHub runners
# (etcd cannot hold localhost:2379), so CI trusts vm-test instead,
# same as the distro's own CI.
#
# Usage: make smoke PODMAN='sudo podman'
#        CLEAN=1 ./hack/smoke-test.sh   # tear down a kept container
set -euo pipefail
# shellcheck source=hack/lib.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib.sh"

IMAGE=${IMAGE:-localhost/stornas-os:dev}
PODMAN=${PODMAN:-podman}
NAME=${NAME:-stornas-smoke}
KCFG=/var/lib/microshift/resources/kubeadmin/kubeconfig
KEEP=${KEEP:-0}

pexec() { $PODMAN exec -i "$NAME" "$@"; }
kc() { pexec kubectl --kubeconfig "$KCFG" "$@"; }

clean() {
	# The pool loop device is a global kernel object; detach it before
	# the container (and its backing file) go away.
	pexec sh -c 'losetup -j /var/lib/smoke-disk.raw | cut -d: -f1 | xargs -r losetup -d' 2>/dev/null || true
	$PODMAN rm -f "$NAME" 2>/dev/null || true
}

diagnostics() {
	log "DIAGNOSTICS: pods"
	kc get pods -A -o wide 2>&1 || true
	log "DIAGNOSTICS: storagepool"
	kc get storagepool smoke -o yaml 2>&1 | tail -30 || true
	log "DIAGNOSTICS: microshift journal (last 40 lines)"
	pexec journalctl -u microshift --no-pager -n 40 2>&1 || true
	log "DIAGNOSTICS: stornas operator and agent logs"
	kc -n stornas-system logs deploy/stornas-operator --tail=30 2>&1 || true
	kc -n stornas-system logs ds/stornas-agent --tail=30 2>&1 || true
}

if [ "${CLEAN:-0}" = 1 ]; then
	clean
	log "cleaned up"
	exit 0
fi

trap 'rc=$?; if [ $rc -ne 0 ]; then diagnostics; fi; [ "$KEEP" = 1 ] || clean; exit $rc' EXIT

sync_rootful_image "$IMAGE" "$PODMAN"

log "starting $NAME from $IMAGE"
$PODMAN rm -f "$NAME" 2>/dev/null || true
$PODMAN run --privileged -d \
	--ulimit nofile=524288:524288 \
	--dns-search=. \
	--tty --volume /dev:/dev \
	--tmpfs /var/lib/containers \
	--name "$NAME" --hostname "$NAME" \
	"$IMAGE" >/dev/null

retry 60 "dbus up" pexec systemctl is-active -q dbus.service
# The embedded-image import (1.5GB unpack, Before=microshift) dominates
# cold start on small runners; wait for it separately so the microshift
# budget measures microshift.
retry 600 "embedded images imported" pexec systemctl is-active -q import-embedded-images.service
retry 600 "microshift service active" pexec systemctl is-active -q microshift.service

log "assert: /var/run is still a symlink (drbd-utils tree regression)"
pexec test -L /var/run || die "/var/run is a real directory"

log "assert: TopoLVM is gone and the stornas greenboot gate is in place"
pexec test ! -e /etc/greenboot/check/required.d/50_microshift_topolvm_check.sh \
	|| die "topolvm greenboot check still present"
pexec test -x /etc/greenboot/check/required.d/50_stornas_storage_check.sh \
	|| die "stornas greenboot check missing"

log "assert: embedded images imported into cri-o"
pexec crictl images | grep -q piraeus-server || die "piraeus-server not imported"
pexec crictl images | grep -q 'epheo/stornas' || die "stornas images not imported"

node_ready() { kc get nodes --no-headers | grep -q ' Ready'; }
retry 300 "node Ready" node_ready

pods_settled() {
	local total bad
	total=$(kc get pods -A --no-headers 2>/dev/null | wc -l)
	bad=$(kc get pods -A --no-headers 2>/dev/null | grep -cvE 'Running|Completed' || true)
	[ "$total" -ge 12 ] && [ "$bad" -eq 0 ]
}
retry 900 "all pods Running/Completed" pods_settled

log "creating a StoragePool on an in-container loop device"
pexec truncate -s 10G /var/lib/smoke-disk.raw
DEV=$(pexec losetup --find --show /var/lib/smoke-disk.raw | tr -d '\r')
kc apply -f - <<EOF
apiVersion: storage.stornas.io/v1alpha1
kind: StoragePool
metadata:
  name: smoke
spec:
  node: $NAME
  devices: ["$DEV"]
  raid: none
EOF
pool_available() { kc get storagepool smoke -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' | grep -q True; }
retry 600 "storage pool Available" pool_available

log "provisioning and writing a PVC through LINSTOR CSI (no drbd layer)"
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: smoke
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
  name: smoke-consumer
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
        claimName: smoke
EOF
pvc_bound() { kc -n stornas-system get pvc smoke -o jsonpath='{.status.phase}' | grep -q Bound; }
consumer_running() { kc -n stornas-system get pod smoke-consumer -o jsonpath='{.status.phase}' | grep -q Running; }
retry 600 "PVC Bound" pvc_bound
retry 300 "consumer pod Running" consumer_running
kc -n stornas-system exec smoke-consumer -- sh -c 'echo probe-ok > /data/probe && cat /data/probe' \
	| grep -q probe-ok || die "PVC write/read failed"

log "UI answers and login works"
retry 120 "healthz on the NodePort" pexec curl -fsS http://127.0.0.1:30080/healthz
ADMIN_PW=$(kc -n stornas-system get secret admin-password -o jsonpath='{.data.password}' | base64 -d)
pexec curl -fsS -c /tmp/smoke-cookies -H 'Content-Type: application/json' \
	-d "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}" \
	http://127.0.0.1:30080/api/v1/login >/dev/null || die "login failed"
pexec curl -fsS -b /tmp/smoke-cookies http://127.0.0.1:30080/api/v1/state \
	| grep -q '"pools":' || die "state missing pools"

log "assert: no failed systemd units"
failed=$(pexec systemctl --failed --no-legend | grep -c . || true)
[ "$failed" -eq 0 ] || { pexec systemctl --failed; die "$failed systemd unit(s) failed"; }

log "SMOKE TEST PASSED"
