#!/usr/bin/env bash
# Two-node replication acceptance test: the first real exercise of the
# stornas-replicated StorageClass and the DRBD kmod beyond modprobe.
#
# One bib build, two qemu VMs booted from copies of the qcow2. Each VM
# has its user-net NIC (ssh hostfwd) plus a second NIC on a qemu socket
# mcast segment: the cluster network (192.168.144.0/24; 10.44.0.0 is
# microshift's default apiserver advertise address on br-ex, so the
# segment must stay out of 10.44.0.0/24). MicroShift joins
# them with its own multinode flow (microshift run --multinode on the
# primary, microshift add-node on the worker - upstream
# scripts/multinode/configure-node.sh is the reference).
#
# The storage assertions: a StoragePool per node, a replicated PVC
# placing DRBD replicas on both, writes surviving the loss of the peer
# node (two replicas cannot form quorum, LINSTOR leaves quorum off, IO
# must continue), and a resync back to UpToDate when the peer returns.
#
# Needs a root-capable podman and qemu-system-x86_64, same as boot-test.
set -euo pipefail

IMAGE=${IMAGE:-localhost/stornas-os:dev}
PODMAN=${PODMAN:-podman}
BIB_IMAGE=${BIB_IMAGE:-quay.io/centos-bootc/bootc-image-builder:latest}
WORKDIR=${WORKDIR:-$(mktemp -d /tmp/stornas-repl.XXXXXX)}
SSH1=${SSH1:-2232}
SSH2=${SSH2:-2233}
VM_MEM=${VM_MEM:-5120}
MCAST=${MCAST:-239.42.0.1:42424}
IP1=192.168.144.1
IP2=192.168.144.2
KEEP=${KEEP:-0}
PIDS=()

log() { printf '\n== %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# ssh adds a remote shell evaluation layer that strips quoting - %q
# every arg (same rationale as boot-test / the distro's vm-test).
nssh() { # nssh <port> <cmd...>
	local port=$1; shift
	ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o LogLevel=ERROR -i "$WORKDIR/id" -p "$port" \
		root@127.0.0.1 "$(printf '%q ' "$@")"
}
v1() { nssh "$SSH1" "$@"; }
v2() { nssh "$SSH2" "$@"; }
kc() { v1 kubectl --kubeconfig /var/lib/microshift/resources/kubeadmin/kubeconfig "$@"; }
linstor_cmd() { kc -n piraeus-datastore exec deploy/linstor-controller -- linstor "$@"; }

diagnostics() {
	log "DIAGNOSTICS: nodes and pods"
	kc get nodes -o wide 2>&1 || true
	kc get pods -A --no-headers 2>&1 | grep -vE 'Running|Completed' || true
	log "DIAGNOSTICS: not-running pod events"
	for p in $(kc get pods -A --no-headers 2>/dev/null \
			| awk '$4 !~ /Running|Completed/ {print $1"/"$2}'); do
		echo "--- ${p}"
		kc -n "${p%/*}" describe pod "${p#*/}" 2>&1 | sed -n '/Events:/,$p' | tail -10 || true
	done
	log "DIAGNOSTICS: kubernetes service endpoints"
	kc get endpoints kubernetes -o wide 2>&1 || true
	log "DIAGNOSTICS: ovn state per node"
	for fn in v1 v2; do
		$fn sh -c 'hostname; ip -br addr show br-ex 2>/dev/null; ip route show default 2>/dev/null; ls /etc/cni/net.d/ 2>/dev/null; echo "-- ovs-init + rehome journals:"; journalctl -u microshift-ovs-init -u rehome-brex --no-pager 2>/dev/null | tail -25' 2>&1 || true
	done
	for p in $(kc -n openshift-ovn-kubernetes get pods -o name 2>/dev/null); do
		echo "--- ${p}"
		kc -n openshift-ovn-kubernetes logs "${p#pod/}" --all-containers --tail=8 2>&1 | tail -10 || true
	done
	log "DIAGNOSTICS: linstor state"
	linstor_cmd node list 2>&1 || true
	linstor_cmd resource list 2>&1 || true
	log "DIAGNOSTICS: consoles (last 15 lines each)"
	tail -15 "$WORKDIR/console1.log" 2>/dev/null || true
	tail -15 "$WORKDIR/console2.log" 2>/dev/null || true
}

cleanup() {
	rc=$?
	[ $rc -ne 0 ] && { log "FAILED (rc=$rc)"; diagnostics; }
	if [ "$KEEP" = 1 ]; then
		log "keeping VMs and $WORKDIR"
		exit $rc
	fi
	for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
	rm -rf "$WORKDIR"
	exit $rc
}
trap cleanup EXIT

retry() { # retry <seconds> <description> <cmd...>
	local deadline=$(( $(date +%s) + $1 )) desc=$2
	printf 'waiting up to %ss for: %s\n' "$1" "$desc"
	shift 2
	until "$@" >/dev/null 2>&1; do
		[ "$(date +%s)" -gt "$deadline" ] && die "timed out waiting for: $desc"
		sleep 5
	done
	log "ok: $desc"
}

mkdir -p "$WORKDIR/output"

log "building qcow2 from $IMAGE"
ssh-keygen -t ed25519 -N '' -f "$WORKDIR/id" -q
cat > "$WORKDIR/config.toml" <<EOF
[[customizations.user]]
name = "root"
key = "$(cat "$WORKDIR/id.pub")"

[[customizations.filesystem]]
mountpoint = "/"
minsize = "30 GiB"
EOF
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
DISK1="$WORKDIR/output/qcow2/disk.qcow2"
[ -f "$DISK1" ] || die "bootc-image-builder produced no qcow2"
DISK2="$WORKDIR/disk2.qcow2"
cp --reflink=auto "$DISK1" "$DISK2"

OVMF_CODE=""
for c in /usr/share/edk2/ovmf/OVMF_CODE.fd /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd; do
	[ -f "$c" ] && OVMF_CODE=$c && break
done
[ -n "$OVMF_CODE" ] || die "no OVMF firmware found (install edk2-ovmf / ovmf)"

boot_vm() { # boot_vm <n> <disk> <ssh-port> <scratch-serial> <mac>
	local n=$1 disk=$2 port=$3 serial=$4 mac=$5
	cp "${OVMF_CODE%CODE*}VARS${OVMF_CODE##*CODE}" "$WORKDIR/vars$n.fd" 2>/dev/null \
		|| cp "$(dirname "$OVMF_CODE")"/OVMF_VARS*.fd "$WORKDIR/vars$n.fd"
	truncate -s 20G "$WORKDIR/scratch$n.raw"
	local accel=tcg
	[ -w /dev/kvm ] && accel=kvm
	qemu-system-x86_64 \
		-machine "q35,accel=$accel" -cpu max -smp 4 -m "$VM_MEM" \
		-drive "if=pflash,format=raw,readonly=on,file=$OVMF_CODE" \
		-drive "if=pflash,format=raw,file=$WORKDIR/vars$n.fd" \
		-drive "file=$disk,if=virtio,format=qcow2" \
		-drive "file=$WORKDIR/scratch$n.raw,if=none,format=raw,id=scratch" \
		-device virtio-blk-pci,drive=scratch,serial="$serial" \
		-netdev "user,id=n0,hostfwd=tcp::${port}-:22" \
		-device virtio-net-pci,netdev=n0 \
		-netdev "socket,id=n1,mcast=$MCAST" \
		-device "virtio-net-pci,netdev=n1,mac=$mac" \
		-device virtio-rng-pci \
		-serial "file:$WORKDIR/console$n.log" \
		-display none -daemonize -pidfile "$WORKDIR/qemu$n.pid"
	PIDS+=("$(cat "$WORKDIR/qemu$n.pid")")
}

log "booting two appliances on a shared cluster segment"
boot_vm 1 "$DISK1" "$SSH1" STORNAS1 52:54:00:44:00:01
boot_vm 2 "$DISK2" "$SSH2" STORNAS2 52:54:00:44:00:02

retry 600 "node1 ssh reachable" v1 true
retry 600 "node2 ssh reachable" v2 true

# First boot must converge before the cluster route change: it pulls
# the OKD release images over user-net, cleanup keeps them local, and
# after the change the only gateway is the (routeless) peer.
settled() { # settled <vssh-fn>
	$1 sh -c 'k="kubectl --kubeconfig /var/lib/microshift/resources/kubeadmin/kubeconfig"; $k get pods -A --no-headers 2>/dev/null | grep -q . || exit 1; $k get pods -A --no-headers | grep -vE "Running|Completed" | grep -q . && exit 1; exit 0'
}
node1_settled() { settled v1; }
node2_settled() { settled v2; }
retry 900 "node1 first boot settled" node1_settled
retry 900 "node2 first boot settled" node2_settled

# The cluster NIC gets a static IP, found by MAC: interface naming is
# not stable across qemu machine types. It also takes the winning
# default route (gateway = the peer, nothing routes off-segment): the
# multinode phase then runs fully offline, which is the air-gap shape
# a real deployment has. MicroShift's br-ex stays virtual and needs no
# rehoming; it never enslaves a physical NIC.
cluster_net() { # cluster_net <vssh-fn> <mac> <ip> <peer-ip>
	local fn=$1 mac=$2 ip=$3 peer=$4
	local dev
	dev=$($fn sh -c "ip -o link | awk -F': ' 'tolower(\$0) ~ /$mac/ {print \$2}'")
	[ -n "$dev" ] || die "cluster NIC with MAC $mac not found"
	$fn nmcli con add type ethernet ifname "$dev" con-name cluster \
		ipv4.method manual ipv4.addresses "$ip/24" \
		ipv4.gateway "$peer" ipv4.route-metric 50 autoconnect yes
	$fn nmcli con up cluster
}
log "configuring the cluster network"
cluster_net v1 52:54:00:44:00:01 "$IP1" "$IP2"
cluster_net v2 52:54:00:44:00:02 "$IP2" "$IP1"
v1 ping -c1 -W3 "$IP2" || die "cluster segment not passing traffic"
# The apiserver reaches kubelets by node name (logs, exec, metrics);
# neither VM resolves the other's hostname without help.
for fn in v1 v2; do
	$fn sh -c "printf '$IP1 node1\n$IP2 node2\n' >> /etc/hosts"
done

# MicroShift multinode, upstream configure-node.sh shape: stop the
# greenboot gate first (cleanup-data fights a running healthcheck),
# distinct hostnames, nodeIP on the cluster NIC, wipe the single-node
# state the image's first boot created, then run --multinode on the
# primary. Each step is logged so an ssh rc=255 locates itself.
node_config() { # node_config <vssh-fn> <hostname> <ip>
	local fn=$1 host=$2 ip=$3
	step() { echo "  [$host] $1"; shift; "$fn" "$@" || die "[$host] failed: $*"; }
	step "stop greenboot" sh -c 'systemctl stop greenboot-healthcheck 2>/dev/null; systemctl reset-failed greenboot-healthcheck 2>/dev/null; systemctl disable greenboot-healthcheck 2>/dev/null; true'
	# Upstream configure-node.sh does the same: the distro firewall only
	# opens apiserver and etcd, not OVN's DB and geneve ports, and
	# ovnkube-node on the joined node crash-loops against them.
	step "stop firewalld" sh -c 'systemctl stop firewalld 2>/dev/null; systemctl disable firewalld 2>/dev/null; true'
	step "set hostname" hostnamectl set-hostname "$host"
	step "write multinode config" sh -c "mkdir -p /etc/microshift/config.d && printf 'node:\n  hostnameOverride: $host\n  nodeIP: $ip\napiServer:\n  subjectAltNames:\n  - $ip\n' > /etc/microshift/config.d/20-multinode.yaml"
	step "wipe single-node state" sh -c 'echo 1 | microshift-cleanup-data --all --keep-images'
	step "multinode unit override" sh -c 'mkdir -p /etc/systemd/system/microshift.service.d && printf "[Service]\nExecStart=\nExecStart=microshift run --multinode\n" > /etc/systemd/system/microshift.service.d/multinode.conf'
	step "daemon-reload" systemctl daemon-reload
}

log "configuring node1 as the multinode primary"
node_config v1 node1 "$IP1"
v1 systemctl enable --now microshift

microshift_ready() { v1 systemctl is-active -q microshift; }
retry 600 "microshift active on node1" microshift_ready
node1_ready() { kc get node node1 --no-headers 2>/dev/null | grep -q ' Ready'; }
retry 600 "node1 Ready" node1_ready

log "joining node2 via microshift add-node"
node_config v2 node2 "$IP2"
BOOTSTRAP="/var/lib/microshift/resources/kubeadmin/$IP1/kubeconfig"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
	-i "$WORKDIR/id" -P "$SSH1" "root@127.0.0.1:$BOOTSTRAP" "$WORKDIR/bootstrap-kubeconfig"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
	-i "$WORKDIR/id" -P "$SSH2" "$WORKDIR/bootstrap-kubeconfig" root@127.0.0.1:/root/bootstrap-kubeconfig
v2 microshift add-node --kubeconfig /root/bootstrap-kubeconfig

both_ready() { [ "$(kc get nodes --no-headers 2>/dev/null | grep -c ' Ready')" -eq 2 ]; }
retry 900 "both nodes Ready" both_ready

satellites_up() { [ "$(kc -n piraeus-datastore get pods -l app.kubernetes.io/component=linstor-satellite --no-headers 2>/dev/null | grep -c Running)" -eq 2 ]; }
retry 900 "two linstor satellites Running" satellites_up

log "creating a StoragePool on each node"
for n in 1 2; do
	kc apply -f - <<EOF
apiVersion: storage.stornas.io/v1alpha1
kind: StoragePool
metadata:
  name: pool-node$n
spec:
  node: node$n
  devices: ["/dev/disk/by-id/virtio-STORNAS$n"]
  raid: none
EOF
done
pools_available() {
	[ "$(kc get storagepool -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Available")].status}{"\n"}{end}' 2>/dev/null | grep -c True)" -eq 2 ]
}
retry 600 "both storage pools Available" pools_available

log "provisioning a replicated PVC, consumer pinned to node1"
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: repl-test
  namespace: stornas-system
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: stornas-replicated
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: repl-consumer
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node1
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: repl-test
EOF
pvc_bound() { kc -n stornas-system get pvc repl-test -o jsonpath='{.status.phase}' | grep -q Bound; }
consumer_up() { kc -n stornas-system get pod repl-consumer -o jsonpath='{.status.phase}' | grep -q Running; }
retry 600 "replicated PVC Bound" pvc_bound
retry 300 "consumer Running on node1" consumer_up

replicas_uptodate() {
	[ "$(linstor_cmd -m resource list 2>/dev/null | grep -o UpToDate | wc -l)" -ge 2 ]
}
retry 300 "two UpToDate DRBD replicas" replicas_uptodate

log "writing through the replicated volume"
kc -n stornas-system exec repl-consumer -- sh -c 'echo before-failover > /data/marker && sync'

log "killing node2: writes must survive the peer loss"
kill "$(cat "$WORKDIR/qemu2.pid")"
sleep 10
kc -n stornas-system exec repl-consumer -- sh -c 'echo during-failover >> /data/marker && sync' \
	|| die "IO blocked after peer loss (quorum should be off with two replicas)"
log "ok: IO continued with the peer down"

log "restarting node2: replica must resync to UpToDate"
boot_vm 2 "$DISK2" "$SSH2" STORNAS2 52:54:00:44:00:02
retry 600 "node2 ssh back" v2 true
retry 600 "both nodes Ready again" both_ready
resynced() {
	out=$(linstor_cmd -m resource list 2>/dev/null) || return 1
	[ "$(grep -o UpToDate <<<"$out" | wc -l)" -ge 2 ] && ! grep -q Inconsistent <<<"$out"
}
retry 900 "replicas resynced to UpToDate" resynced

log "data intact after failover cycle"
kc -n stornas-system exec repl-consumer -- cat /data/marker | grep -q during-failover \
	|| die "marker written during failover is missing"

log "REPLICATION TEST PASSED"
