#!/usr/bin/env bash
# Two-node replication acceptance test: the first real exercise of the
# stornas-replicated StorageClass and the DRBD kmod beyond modprobe.
# The join is the distro's worker role (microshift-profile worker +
# add-node --worker); firewalld and greenboot stay in place, so this
# also proves the stornas firewall service and the role-aware check.
#
# Network shape follows upstream microshift's multinode docs exactly:
# ONE network per VM that carries ssh, cluster traffic, and internet -
# there it is libvirt's NAT bridge; here a host bridge with dnsmasq
# (DHCP with per-MAC reservations) and MASQUERADE, taps enslaved. The
# earlier split design (user-net for ssh, an isolated mcast segment
# for the cluster) broke OVN in ways multinode was never built to
# survive. Also: keep every guest subnet out of 10.42-10.44; 10.44.0.0
# is microshift's apiserver advertise address on br-ex.
#
# The storage assertions: a StoragePool per node, a replicated PVC
# placing DRBD replicas on both, writes surviving the loss of the peer
# node (two replicas cannot form quorum, LINSTOR leaves quorum off, IO
# must continue), and a resync back to UpToDate when the peer returns.
#
# The same kill cycle carries the failover assertions (failure matrix:
# "node dies, replicated volume"): a Target and a Share active on the
# worker must re-place to the controller with the VIP answering there,
# and the returned worker must be fenced clean (no export, no VIP, no
# mount). Killing the controller is a different matrix row: no operator
# means no re-placement, only surviving IO.
#
# After the failover cycle: real client IO (NFS mount, iSCSI CHAP login)
# against the moved exports, the 2-node split-brain row (bridge port
# isolation, forced divergence, pick-survivor through the API), and the
# controller-down row (IO continues, provisioning blocks and recovers).
#
# Needs a root-capable podman, qemu-system-x86_64, dnsmasq, and sudo
# for the bridge/taps. CI-sized: two 5GB VMs.
set -euo pipefail
# shellcheck source=hack/lib.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib.sh"

IMAGE=${IMAGE:-localhost/stornas-os:dev}
PODMAN=${PODMAN:-podman}
BIB_IMAGE=${BIB_IMAGE:-quay.io/centos-bootc/bootc-image-builder:latest}
WORKDIR=${WORKDIR:-$(mktemp -d /tmp/stornas-repl.XXXXXX)}
VM_MEM=${VM_MEM:-5120}
BR=stornas-br0
NET=192.168.100
IP1=$NET.11
IP2=$NET.12
MAC1=52:54:00:44:00:01
MAC2=52:54:00:44:00:02
KEEP=${KEEP:-0}
PIDS=()

# ssh adds a remote shell evaluation layer that strips quoting - %q
# every arg (same rationale as boot-test / the distro's vm-test).
nssh() { # nssh <ip> <cmd...>
	local ip=$1; shift
	ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o LogLevel=ERROR -i "$WORKDIR/id" \
		"root@$ip" "$(printf '%q ' "$@")"
}
v1() { nssh "$IP1" "$@"; }
v2() { nssh "$IP2" "$@"; }
kc() { v1 kubectl --kubeconfig /var/lib/microshift/resources/kubeadmin/kubeconfig "$@"; }
linstor_cmd() { kc -n piraeus-datastore exec deploy/linstor-controller -- linstor "$@"; }

host_net_up() {
	sudo ip link add "$BR" type bridge 2>/dev/null || true
	sudo ip addr replace "$NET.1/24" dev "$BR"
	sudo ip link set "$BR" up
	sudo sysctl -qw net.ipv4.ip_forward=1
	sudo iptables -t nat -C POSTROUTING -s "$NET.0/24" ! -o "$BR" -j MASQUERADE 2>/dev/null \
		|| sudo iptables -t nat -A POSTROUTING -s "$NET.0/24" ! -o "$BR" -j MASQUERADE
	sudo iptables -C FORWARD -i "$BR" -j ACCEPT 2>/dev/null || sudo iptables -I FORWARD -i "$BR" -j ACCEPT
	sudo iptables -C FORWARD -o "$BR" -j ACCEPT 2>/dev/null || sudo iptables -I FORWARD -o "$BR" -j ACCEPT
	for t in tap-stornas1 tap-stornas2; do
		sudo ip tuntap add "$t" mode tap 2>/dev/null || true
		sudo ip link set "$t" master "$BR" up
	done
	sudo dnsmasq --interface="$BR" --bind-interfaces --except-interface=lo \
		--dhcp-range="$NET.10,$NET.50,12h" \
		--dhcp-host="$MAC1,$IP1" --dhcp-host="$MAC2,$IP2" \
		--pid-file="$WORKDIR/dnsmasq.pid" || die "dnsmasq failed to start"
}

host_net_down() {
	sudo iptables -D FORWARD -m physdev --physdev-in tap-stornas1 --physdev-out tap-stornas2 -j DROP 2>/dev/null || true
	sudo iptables -D FORWARD -m physdev --physdev-in tap-stornas2 --physdev-out tap-stornas1 -j DROP 2>/dev/null || true
	sudo kill "$(cat "$WORKDIR/dnsmasq.pid" 2>/dev/null)" 2>/dev/null || true
	for t in tap-stornas1 tap-stornas2; do sudo ip link del "$t" 2>/dev/null || true; done
	sudo ip link del "$BR" 2>/dev/null || true
	sudo iptables -t nat -D POSTROUTING -s "$NET.0/24" ! -o "$BR" -j MASQUERADE 2>/dev/null || true
	sudo iptables -D FORWARD -i "$BR" -j ACCEPT 2>/dev/null || true
	sudo iptables -D FORWARD -o "$BR" -j ACCEPT 2>/dev/null || true
}

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
		$fn sh -c 'hostname; ip -br addr show br-ex 2>/dev/null; ip route show default 2>/dev/null; ls /etc/cni/net.d/ 2>/dev/null' 2>&1 || true
	done
	log "DIAGNOSTICS: node2 apiserver reachability (advertise address)"
	v2 sh -c 'ss -ltn | grep 6443 || echo "nothing listening on 6443"; curl -ks -m3 https://10.44.0.0:6443/healthz || echo " (advertise address unreachable)"; systemctl is-active microshift' 2>&1 || true
	log "DIAGNOSTICS: node2 ovnkube-controller log"
	p2=$(kc -n openshift-ovn-kubernetes get pods -l app=ovnkube-node -o wide --no-headers 2>/dev/null | awk '$7=="node2" {print $1}')
	[ -n "${p2:-}" ] && kc -n openshift-ovn-kubernetes logs "$p2" -c ovnkube-controller --tail=60 2>&1 \
		| grep -iE 'error|fail|wait|timed|zone|transit|gateway' | tail -25 || true
	log "DIAGNOSTICS: node2 microshift journal errors"
	v2 sh -c 'journalctl -u microshift --no-pager -p err -n 20' 2>&1 || true
	log "DIAGNOSTICS: linstor state"
	linstor_cmd node list 2>&1 || true
	linstor_cmd storage-pool list 2>&1 || true
	linstor_cmd resource list 2>&1 || true
	linstor_cmd err list 2>&1 | tail -12 || true
	log "DIAGNOSTICS: DRBD kernel view per node"
	for fn in v1 v2; do
		$fn sh -c 'hostname; drbdsetup status --verbose 2>/dev/null; dmesg | grep -i drbd | tail -25' 2>&1 || true
	done
	log "DIAGNOSTICS: CSI capacity view"
	kc get csistoragecapacities -A 2>&1 || true
	kc -n stornas-system describe pvc repl-test 2>&1 | sed -n '/Events:/,$p' | tail -8 || true
	kc -n piraeus-datastore logs deploy/linstor-csi-controller -c csi-provisioner --tail=25 2>&1 \
		| grep -iE 'error|fail|capacity' | tail -12 || true
	log "DIAGNOSTICS: failover placement and host state"
	kc -n stornas-system get target,share failover -o yaml 2>&1 | grep -A30 'status:' | tail -80 || true
	for fn in v1 v2; do
		$fn sh -c "hostname; ip -j addr show to ${VIP:-0.0.0.0} 2>/dev/null; targetcli ls /iscsi 1 2>/dev/null; exportfs -v 2>/dev/null" 2>&1 || true
	done
	log "DIAGNOSTICS: stornas control plane placement and operator log"
	kc -n stornas-system get pods -o wide 2>&1 || true
	kc -n stornas-system logs deploy/stornas-operator --tail=30 2>&1 || true
	log "DIAGNOSTICS: NFS server view on node1"
	v1 sh -c 'cat /var/lib/nfs/etab 2>/dev/null; cat /proc/fs/nfsd/versions 2>/dev/null; ls -Z /var/lib/stornas/shares 2>/dev/null' 2>&1 || true
	log "DIAGNOSTICS: one verbose NFS mount attempt from node2"
	v2 sh -c "mkdir -p /mnt/nfs-diag && mount -t nfs -o nfsvers=4.2 ${IP1:-127.0.0.1}:/stornas-system-failover /mnt/nfs-diag; umount /mnt/nfs-diag 2>/dev/null; true" 2>&1 || true
	log "DIAGNOSTICS: who owns 2049 on node1, kernel export cache, mountd log"
	v1 sh -c 'ss -tlnp | grep 2049; cat /proc/net/rpc/nfsd.export/content 2>/dev/null; journalctl -u nfs-mountd --no-pager -n 15 2>/dev/null' 2>&1 || true
	log "DIAGNOSTICS: self-mount on node1 and pseudo-root walk from node2"
	v1 sh -c 'mkdir -p /mnt/nfs-self && mount -t nfs -o nfsvers=4.2 localhost:/stornas-system-failover /mnt/nfs-self && echo self-mount-ok; umount /mnt/nfs-self 2>/dev/null; true' 2>&1 || true
	v2 sh -c "mkdir -p /mnt/nfs-root && mount -t nfs -o nfsvers=4.2 ${IP1:-127.0.0.1}:/ /mnt/nfs-root && find /mnt/nfs-root -maxdepth 5 2>/dev/null | head -15; umount /mnt/nfs-root 2>/dev/null; true" 2>&1 || true
	log "DIAGNOSTICS: iSCSI target tree, auth attributes, one verbose login"
	v1 sh -c "targetcli ls /iscsi/${TIQN:-none} 3 2>/dev/null; targetcli /iscsi/${TIQN:-none}/tpg1 get attribute authentication generate_node_acls demo_mode_write_protect 2>/dev/null; targetcli /iscsi/${TIQN:-none}/tpg1/acls/${INI:-none} get auth 2>/dev/null" 2>&1 || true
	v2 sh -c "iscsiadm -m discovery -t sendtargets -p ${VIP:-127.0.0.1} 2>&1; iscsiadm -m node -T ${TIQN:-none} -p ${VIP:-127.0.0.1}:3260 -o update -n node.session.auth.authmethod -v CHAP 2>&1; iscsiadm -m node -T ${TIQN:-none} -p ${VIP:-127.0.0.1}:3260 -o update -n node.session.auth.username -v e2e 2>&1; iscsiadm -m node -T ${TIQN:-none} -p ${VIP:-127.0.0.1}:3260 -o update -n node.session.auth.password -v e2echappass123 2>&1; iscsiadm -m node -T ${TIQN:-none} -p ${VIP:-127.0.0.1}:3260 --login 2>&1; iscsiadm -m session 2>&1; dmesg | grep -iE 'iscsi|chap' | tail -8" 2>&1 || true
	log "DIAGNOSTICS: SMB state on node1"
	v1 sh -c 'systemctl is-active smb; pdbedit -L; id e2esmb; cat /etc/samba/stornas-shares.conf; testparm -s 2>/dev/null | sed -n "/\[failover\]/,\$p"' 2>&1 || true
	log "DIAGNOSTICS: agent logs per node"
	for p in $(kc -n stornas-system get pods -l app.kubernetes.io/name=stornas-agent -o name 2>/dev/null); do
		echo "--- ${p}"
		kc -n stornas-system logs "${p#pod/}" --tail=25 2>&1 | grep -iE 'smb|useradd|localuser|error' | tail -10 || true
	done
	log "DIAGNOSTICS: verbose smbclient from node2"
	v2 sh -c "smbclient //${IP1:-127.0.0.1}/failover -U e2esmb%e2esmbpass123 -d 3 -c exit" 2>&1 | tail -20 || true
	log "DIAGNOSTICS: AVC denials on node1"
	v1 sh -c 'ausearch -m avc -ts recent 2>/dev/null | tail -15 || dmesg | grep -i avc | tail -15' 2>&1 || true
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
	for p in "${PIDS[@]}"; do sudo kill "$p" 2>/dev/null || true; done
	sleep 1
	host_net_down
	sudo rm -rf "$WORKDIR"
	exit $rc
}
trap cleanup EXIT

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
DISK1="$WORKDIR/output/qcow2/disk.qcow2"
[ -f "$DISK1" ] || die "bootc-image-builder produced no qcow2"
DISK2="$WORKDIR/disk2.qcow2"
cp --reflink=auto "$DISK1" "$DISK2"

OVMF_CODE=""
for c in /usr/share/edk2/ovmf/OVMF_CODE.fd /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd; do
	[ -f "$c" ] && OVMF_CODE=$c && break
done
[ -n "$OVMF_CODE" ] || die "no OVMF firmware found (install edk2-ovmf / ovmf)"

log "creating the shared NAT bridge network"
host_net_up

boot_vm() { # boot_vm <n> <disk> <scratch-serial> <mac> <tap>
	local n=$1 disk=$2 serial=$3 mac=$4 tap=$5
	cp "${OVMF_CODE%CODE*}VARS${OVMF_CODE##*CODE}" "$WORKDIR/vars$n.fd" 2>/dev/null \
		|| cp "$(dirname "$OVMF_CODE")"/OVMF_VARS*.fd "$WORKDIR/vars$n.fd"
	truncate -s 20G "$WORKDIR/scratch$n.raw"
	local accel=tcg
	[ -w /dev/kvm ] && accel=kvm
	sudo qemu-system-x86_64 \
		-machine "q35,accel=$accel" -cpu max -smp 4 -m "$VM_MEM" \
		-drive "if=pflash,format=raw,readonly=on,file=$OVMF_CODE" \
		-drive "if=pflash,format=raw,file=$WORKDIR/vars$n.fd" \
		-drive "file=$disk,if=virtio,format=qcow2" \
		-drive "file=$WORKDIR/scratch$n.raw,if=none,format=raw,id=scratch" \
		-device virtio-blk-pci,drive=scratch,serial="$serial" \
		-netdev "tap,id=n0,ifname=$tap,script=no,downscript=no" \
		-device "virtio-net-pci,netdev=n0,mac=$mac" \
		-device virtio-rng-pci \
		-serial "file:$WORKDIR/console$n.log" \
		-display none -daemonize -pidfile "$WORKDIR/qemu$n.pid"
	PIDS+=("$(sudo cat "$WORKDIR/qemu$n.pid")")
}

log "booting two appliances on the shared network"
boot_vm 1 "$DISK1" STORNAS1 "$MAC1" tap-stornas1
boot_vm 2 "$DISK2" STORNAS2 "$MAC2" tap-stornas2

retry 600 "node1 ssh reachable" v1 true
retry 600 "node2 ssh reachable" v2 true

# First boot pulls the OKD release images; cleanup keeps them local.
settled() { # settled <vssh-fn>
	$1 sh -c 'k="kubectl --kubeconfig /var/lib/microshift/resources/kubeadmin/kubeconfig"; $k get pods -A --no-headers 2>/dev/null | grep -q . || exit 1; $k get pods -A --no-headers | grep -vE "Running|Completed" | grep -q . && exit 1; exit 0'
}
node1_settled() { settled v1; }
node2_settled() { settled v2; }
retry 900 "node1 first boot settled" node1_settled
retry 900 "node2 first boot settled" node2_settled

# The apiserver reaches kubelets by node name (logs, exec, metrics).
for fn in v1 v2; do
	$fn sh -c "printf '$IP1 node1\n$IP2 node2\n' >> /etc/hosts"
done

# Distro worker-role flow (multinode-test.sh in epheo/microshift):
# firewalld and greenboot stay in place; greenboot pauses only around
# cleanup-data (a running health check fights the wipe). Each step is
# logged so an ssh rc=255 locates itself.
node_config() { # node_config <vssh-fn> <hostname> <ip>
	local fn=$1 host=$2 ip=$3
	step() { echo "  [$host] $1"; shift; "$fn" "$@" || die "[$host] failed: $*"; }
	step "pause greenboot around the wipe" sh -c 'systemctl stop greenboot-healthcheck 2>/dev/null; systemctl reset-failed greenboot-healthcheck 2>/dev/null; true'
	step "set hostname" hostnamectl set-hostname "$host"
	step "write node config" sh -c "mkdir -p /etc/microshift/config.d && printf 'node:\n  hostnameOverride: $host\n  nodeIP: $ip\napiServer:\n  subjectAltNames:\n  - $ip\n' > /etc/microshift/config.d/20-multinode.yaml"
	step "wipe single-node state" sh -c 'echo 1 | microshift-cleanup-data --all --keep-images'
}

log "configuring node1 as the controller"
node_config v1 node1 "$IP1"
v1 systemctl enable --now microshift

microshift_ready() { v1 systemctl is-active -q microshift; }
retry 600 "microshift active on node1" microshift_ready
node1_ready() { kc get node node1 --no-headers 2>/dev/null | grep -q ' Ready'; }
retry 600 "node1 Ready" node1_ready

log "joining node2 as a worker (microshift-profile worker + add-node --worker)"
node_config v2 node2 "$IP2"
v2 microshift-profile worker
v2 systemctl enable microshift
BOOTSTRAP="/var/lib/microshift/resources/kubeadmin/$IP1/kubeconfig"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
	-i "$WORKDIR/id" "root@$IP1:$BOOTSTRAP" "$WORKDIR/bootstrap-kubeconfig"
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
	-i "$WORKDIR/id" "$WORKDIR/bootstrap-kubeconfig" "root@$IP2:/root/bootstrap-kubeconfig"
v2 microshift add-node --worker --kubeconfig /root/bootstrap-kubeconfig

both_ready() { [ "$(kc get nodes --no-headers 2>/dev/null | grep -c ' Ready')" -eq 2 ]; }
retry 900 "both nodes Ready" both_ready
kc get node node2 -o jsonpath='{.metadata.labels}' | grep -q 'node-role.kubernetes.io/worker' \
	|| die "node2 is missing the worker role label"

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

# The scheduler's CSI capacity check sits between pool and PVC; assert
# the raw LINSTOR view first so a capacity failure names itself.
pools_have_capacity() {
	out=$(linstor_cmd -m storage-pool list -s stornas 2>/dev/null) || return 1
	[ "$(grep -oE '"free_capacity": *[1-9][0-9]*' <<<"$out" | wc -l)" -ge 2 ]
}
retry 120 "LINSTOR reports free capacity on both pools" pools_have_capacity

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
  # The agent SA's privileged SCC lets the consumer write the volume as
  # root; restricted-v2 left the mount root-owned (no fsGroup applied).
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
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

VIP=$NET.60
# The appliance API and browser drive every mutation from here on, as a
# user would; kubectl stays for cluster-level scaffolding only.
ADMIN_PW=$(kc -n stornas-system get secret admin-password -o jsonpath='{.data.password}' | base64 -d)
api_login() {
	curl -fsS -m 15 -c "$WORKDIR/cookies" -H 'Content-Type: application/json' \
		-d "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}" \
		"http://$IP1:30080/api/v1/login" >/dev/null
}
api() { # api <method> <path> [json body]
	if [ $# -ge 3 ]; then
		curl -fsS -m 15 -b "$WORKDIR/cookies" -X "$1" -H 'Content-Type: application/json' \
			-d "$3" "http://$IP1:30080/api/v1$2"
	else
		curl -fsS -m 15 -b "$WORKDIR/cookies" -X "$1" "http://$IP1:30080/api/v1$2"
	fi
}
retry 60 "appliance API login" api_login
# Real-browser phases; need playwright's chromium.
ui_phase() { # ui_phase <phase>
	UI_URL="http://$IP1:30080" ADMIN_PW="$ADMIN_PW" REPL_VIP="$VIP" \
		REPL_VIP_CIDR="$VIP/24" REPL_NFS_CLIENTS="$NET.0/24(rw,no_root_squash)" \
		REPL_NFS="$IP1:/stornas-system-failover" SURVIVOR=node1 \
		node web/e2e/ui.mjs "$1" || die "UI phase $1 failed"
}

log "failover setup: a Target and a Share active on node2"
# Placement follows the DRBD primary and the primary is the first opener,
# so a consumer pinned to node2 steers initial placement; the agent's own
# open (LIO backstore, share mount) keeps the device primary after the
# steering pod goes.
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: lun0
  namespace: stornas-system
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  storageClassName: stornas-replicated
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: share0
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
  name: steer-lun0
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node2
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
      volumeDevices:
        - name: v
          devicePath: /dev/steer
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: lun0
---
apiVersion: v1
kind: Pod
metadata:
  name: steer-share0
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node2
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: share0
EOF
# A local volume on node2 rides along: the matrix row "node dies, local
# volume" needs one to exist when the node goes down.
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: local-n2
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
  name: local-n2-consumer
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node2
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: local-n2
EOF
local_bound() { kc -n stornas-system get pvc local-n2 -o jsonpath='{.status.phase}' | grep -q Bound; }
retry 300 "local volume Bound on node2" local_bound

# The reattach row of the matrix (the hyperconverged story): a consumer
# on the doomed node must come back on the survivor with its data.
log "reattach setup: replicated PVC with its consumer on node2"
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: reattach
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
  name: reattach-consumer
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node2
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: reattach
EOF
reattach_up() { kc -n stornas-system get pod reattach-consumer -o jsonpath='{.status.phase}' | grep -q Running; }
retry 600 "reattach consumer Running on node2" reattach_up
kc -n stornas-system exec reattach-consumer -- sh -c 'echo before-node-loss > /data/marker && sync'

steered() { # steered <pvc>
	h=$(kc -n stornas-system get pvc "$1" -o jsonpath='{.spec.volumeName}' 2>/dev/null) || return 1
	[ -n "$h" ] || return 1
	linstor_cmd resource list -r "$h" 2>/dev/null | grep node2 | grep -q InUse
}
lun0_steered() { steered lun0; }
share0_steered() { steered share0; }
retry 600 "lun0 primary on node2" lun0_steered
retry 600 "share0 primary on node2" share0_steered

log "creating the target and share through the UI dialogs"
ui_phase create-exports
target_on() { kc -n stornas-system get target failover -o jsonpath='{.status.activeNode} {.status.state}' | grep -qx "$1 Exported"; }
share_on() { kc -n stornas-system get share failover -o jsonpath='{.status.node} {.status.state}' | grep -qx "$1 Exported"; }
target_placed() { kc -n stornas-system get target failover -o jsonpath='{.status.activeNode}' | grep -qx node2; }
share_placed() { kc -n stornas-system get share failover -o jsonpath='{.status.node}' | grep -qx node2; }
retry 120 "target placed on node2" target_placed
retry 120 "share placed on node2" share_placed

# The steering pods hold the device and filesystem open, which blocks the
# agent's own exclusive opens (LIO backstore, xfs mount). Placement is
# sticky, so dropping them now keeps node2 while freeing the devices.
kc -n stornas-system delete pod steer-lun0 steer-share0 --wait

target_on_node2() { target_on node2; }
share_on_node2() { share_on node2; }
retry 300 "target exported from node2" target_on_node2
retry 300 "share exported from node2" share_on_node2
vip_up() { # vip_up <vssh-fn>
	$1 sh -c "ip -j addr show to $VIP | grep -q ifname"
}
vip_on_node2() { vip_up v2; }
retry 120 "VIP held by node2" vip_on_node2
# The VIP must answer off-node, which is what the GARP buys.
vip_answers() { timeout 3 bash -c "</dev/tcp/$VIP/3260"; }
retry 60 "iSCSI reachable on the VIP from the host" vip_answers

log "killing node2: writes must survive the peer loss"
sudo kill "$(sudo cat "$WORKDIR/qemu2.pid")"
sleep 10
kc -n stornas-system exec repl-consumer -- sh -c 'echo during-failover >> /data/marker && sync' \
	|| die "IO blocked after peer loss (quorum should be off with two replicas)"
log "ok: IO continued with the peer down"

# Failover: once node2 goes NotReady the operator must re-place both
# exports to node1 and the agent there must raise the VIP and answer.
target_on_node1() { target_on node1; }
share_on_node1() { share_on node1; }
retry 300 "target re-placed and exported from node1" target_on_node1
retry 300 "share re-placed and exported from node1" share_on_node1
vip_on_node1() { vip_up v1; }
retry 120 "VIP moved to node1" vip_on_node1
retry 60 "iSCSI reachable on the moved VIP from the host" vip_answers
share_served_node1() { v1 sh -c 'exportfs | grep -q stornas-system-failover'; }
retry 60 "NFS export live on node1" share_served_node1

log "UI states the dead node and its stranded local volume"
ui_phase node-down

# The k8s non-graceful shutdown flow: the pod object on the dead node is
# force-removed and the out-of-service taint releases its attachments;
# only then can the survivor mount the replicated volume.
log "consumer reattaches on the survivor with its data"
kc -n stornas-system delete pod reattach-consumer --force --grace-period=0 --wait=false
kc taint node node2 node.kubernetes.io/out-of-service=nodeshutdown:NoExecute
kc apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: reattach-consumer2
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node1
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: reattach
EOF
reattach2_up() { kc -n stornas-system get pod reattach-consumer2 -o jsonpath='{.status.phase}' | grep -q Running; }
retry 600 "consumer reattached on node1" reattach2_up
kc -n stornas-system exec reattach-consumer2 -- grep -q before-node-loss /data/marker \
	|| die "data missing after the reattach"
kc -n stornas-system exec reattach-consumer2 -- sh -c 'echo after-reattach >> /data/marker && sync' \
	|| die "reattached volume refuses writes"
# The taint is the admin's statement that the node is down; it must go
# before the node returns or it evicts everything that lands there.
kc taint node node2 node.kubernetes.io/out-of-service-

log "restarting node2: replica must resync to UpToDate"
boot_vm 2 "$DISK2" STORNAS2 "$MAC2" tap-stornas2
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

# Fencing: the returned ex-primary must shed everything it served, even
# what target.service restored from saveconfig at boot, and placement
# must stay put on node1.
node2_shed_export() { v2 sh -c 'targetcli ls /iscsi 1 2>/dev/null | grep -q stornas:failover && exit 1; exit 0'; }
node2_shed_vip() { v2 sh -c "ip -j addr show to $VIP | grep -q ifname && exit 1; exit 0"; }
node2_shed_mount() { v2 sh -c 'findmnt /var/lib/stornas/shares/stornas-system-failover >/dev/null 2>&1 && exit 1; exit 0'; }
retry 300 "node2 shed the iSCSI export" node2_shed_export
retry 120 "node2 shed the VIP" node2_shed_vip
retry 120 "node2 shed the share mount" node2_shed_mount
target_on node1 || die "target moved off node1 after node2 returned"
share_on node1 || die "share moved off node1 after node2 returned"
retry 60 "iSCSI still reachable on the VIP" vip_answers

# Real clients against the failed-over exports: reachability proves the
# wiring, only IO proves the product.
log "NFS client IO from node2 against the node1 export"
# vers pinned: only 2049/tcp is open, so v4 is the supported protocol and
# a silent v3 fallback would produce misleading mountd errors. The mount
# path is relative to the fsid=0 pseudo root (image Containerfile): the
# composefs rootfs cannot anchor the v4 tree.
nfs_io() {
	v2 sh -c "mkdir -p /mnt/nfs-e2e && mount -t nfs -o nfsvers=4.2 $IP1:/stornas-system-failover /mnt/nfs-e2e && echo nfs-io > /mnt/nfs-e2e/probe && sync && grep -q nfs-io /mnt/nfs-e2e/probe && umount /mnt/nfs-e2e"
}
retry 120 "NFS client wrote and read back" nfs_io

# The client list is live access control: narrowing it must lock the
# client out without a re-export cycle, restoring it must let it back.
log "narrowing the NFS client list takes effect live"
kc -n stornas-system patch share failover --type merge \
	-p '{"spec":{"nfs":{"clients":["10.99.0.0/24(rw)"]}}}'
# v4 pseudo-fs semantics: the out-of-list mount can "succeed" onto the
# shadowed empty mountpoint of the ro root without ever crossing into
# the share's filesystem. The property that matters: the share's data
# (the probe nfs_io wrote) is invisible and writes are refused.
nfs_denied() {
	v2 sh -c "umount /mnt/nfs-e2e 2>/dev/null; mkdir -p /mnt/nfs-e2e; \
		if ! mount -t nfs -o nfsvers=4.2 $IP1:/stornas-system-failover /mnt/nfs-e2e; then exit 0; fi; \
		rc=0; \
		if [ -e /mnt/nfs-e2e/probe ]; then rc=1; fi; \
		if echo x > /mnt/nfs-e2e/denied 2>/dev/null; then rc=1; fi; \
		umount /mnt/nfs-e2e; exit \$rc"
}
retry 120 "client outside the list cannot reach the data" nfs_denied
kc -n stornas-system patch share failover --type merge \
	-p "{\"spec\":{\"nfs\":{\"clients\":[\"$NET.0/24(rw,no_root_squash)\"]}}}"
retry 120 "restored client list mounts again" nfs_io

# SMB rides the same share: user via the appliance API, passdb by the
# agent, login and IO by a real client. Wrong passwords and deleted
# users must fail: the passdb is a security boundary.
log "SMB user through the API, client IO from node2"
api POST /users '{"name":"e2esmb","password":"e2esmbpass123","role":"viewer","smb":true}' >/dev/null \
	|| die "user create through the API failed"
# Provisioning asserted apart from IO so a failure names its layer.
smb_provisioned() { v1 sh -c 'pdbedit -L | grep -q e2esmb'; }
retry 180 "e2esmb in the node1 passdb" smb_provisioned
smb_io() {
	v2 sh -c "echo smb-io > /tmp/smb-probe \
		&& smbclient //$IP1/failover -U e2esmb%e2esmbpass123 \
			-c 'put /tmp/smb-probe probe.txt; get probe.txt /tmp/smb-back' \
		&& grep -q smb-io /tmp/smb-back"
}
retry 300 "SMB client wrote and read back" smb_io
v2 sh -c "smbclient //$IP1/failover -U e2esmb%wrongpass -c exit" \
	&& die "SMB login succeeded with a wrong password"
api DELETE /users/e2esmb >/dev/null || die "user delete through the API failed"
smb_revoked() { ! v2 sh -c "smbclient //$IP1/failover -U e2esmb%e2esmbpass123 -c exit"; }
retry 120 "SMB access revoked after user delete" smb_revoked
log "ok: SMB IO, wrong-password refusal, and revocation on delete"

log "iSCSI client login with CHAP from node2 via the VIP"
TIQN=iqn.2026-08.io.stornas:failover
# iscsi-init generates the IQN on first boot only when its unit runs;
# generate directly when absent.
v2 sh -c '[ -f /etc/iscsi/initiatorname.iscsi ] || printf "InitiatorName=%s\n" "$(iscsi-iname)" > /etc/iscsi/initiatorname.iscsi'
INI=$(v2 sh -c "sed -n 's/^InitiatorName=//p' /etc/iscsi/initiatorname.iscsi")
[ -n "$INI" ] || die "node2 has no initiator IQN"
kc -n stornas-system create secret generic e2e-chap \
	--from-literal=username=e2e --from-literal=password=e2echappass123
kc -n stornas-system patch target failover --type merge \
	-p "{\"spec\":{\"initiators\":[{\"iqn\":\"$INI\",\"chapSecretRef\":\"e2e-chap\"}]}}"
v2 systemctl start iscsid
iscsi_login() {
	v2 sh -c "iscsiadm -m discovery -t sendtargets -p $VIP >/dev/null \
		&& iscsiadm -m node -T $TIQN -p $VIP:3260 -o update -n node.session.auth.authmethod -v CHAP \
		&& iscsiadm -m node -T $TIQN -p $VIP:3260 -o update -n node.session.auth.username -v e2e \
		&& iscsiadm -m node -T $TIQN -p $VIP:3260 -o update -n node.session.auth.password -v e2echappass123 \
		&& iscsiadm -m node -T $TIQN -p $VIP:3260 --login"
}
# The first attempts race the agent converging the new ACL.
retry 120 "iSCSI login via the VIP" iscsi_login
LUN_DEV="/dev/disk/by-path/ip-$VIP:3260-iscsi-$TIQN-lun-0"
lun_dev_up() { v2 test -b "$LUN_DEV"; }
retry 60 "LUN device visible on node2" lun_dev_up
v2 sh -c "dd if=/dev/urandom of=/tmp/probe bs=512 count=1 2>/dev/null \
	&& dd if=/tmp/probe of=$LUN_DEV bs=512 count=1 oflag=direct 2>/dev/null \
	&& dd if=$LUN_DEV bs=512 count=1 iflag=direct 2>/dev/null | cmp - /tmp/probe" \
	|| die "iSCSI block IO mismatch"
v2 iscsiadm -m node -T "$TIQN" -p "$VIP:3260" --logout
log "ok: iSCSI client wrote and read back through the VIP"

# Rotation is timer-based (nothing watches Secrets); the new password
# must land within secretRefreshInterval and the old one must die.
log "CHAP rotation: new secret lands, old one stops working"
kc -n stornas-system patch secret e2e-chap --type merge \
	-p '{"stringData":{"password":"rotatedchappass1"}}'
iscsi_login_rotated() {
	v2 sh -c "iscsiadm -m node -T $TIQN -p $VIP:3260 -o update -n node.session.auth.password -v rotatedchappass1 \
		&& iscsiadm -m node -T $TIQN -p $VIP:3260 --login"
}
retry 400 "iSCSI login with the rotated CHAP secret" iscsi_login_rotated
v2 iscsiadm -m node -T "$TIQN" -p "$VIP:3260" --logout
v2 sh -c "iscsiadm -m node -T $TIQN -p $VIP:3260 -o update -n node.session.auth.password -v e2echappass123 \
	&& iscsiadm -m node -T $TIQN -p $VIP:3260 --login" \
	&& die "retired CHAP secret still accepted"

log "resize, snapshot, restore on the replicated volume through the UI dialogs"
TARGET_VOL=repl-test TARGET_SIZE=2Gi ui_phase resize-volume
repl_resized() { kc -n stornas-system get pvc repl-test -o jsonpath='{.status.capacity.storage}' | grep -qx 2Gi; }
retry 300 "replicated PVC grew to 2Gi" repl_resized
kc -n stornas-system exec repl-consumer -- sh -c 'df -k /data | tail -1' \
	| awk '{exit ($2 > 1900000) ? 0 : 1}' || die "filesystem did not grow with the replicated volume"
TARGET_VOL=repl-test TARGET_SNAP=repl-snap ui_phase snapshot-volume
repl_snap_ready() { kc -n stornas-system get volumesnapshot repl-snap -o jsonpath='{.status.readyToUse}' | grep -q true; }
retry 300 "replicated snapshot ready" repl_snap_ready
TARGET_SNAP=repl-snap TARGET_VOL=repl-restore ui_phase restore-snapshot
# The consumer lands inside the binder's grace, so pod-first placement
# pins the restore to node1; the podless restore is boot-test's row.
kc apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: repl-restore-consumer
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node1
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: repl-restore
EOF
restore_bound() { kc -n stornas-system get pvc repl-restore -o jsonpath='{.status.phase}' | grep -q Bound; }
retry 300 "restored volume Bound" restore_bound
# Bound is the claim, not the pod: exec needs the container up.
restore_up() { kc -n stornas-system get pod repl-restore-consumer -o jsonpath='{.status.phase}' | grep -q Running; }
retry 300 "restore consumer Running" restore_up
# The restore must stay replicated, not fall to the default local class.
restore_class=$(kc -n stornas-system get pvc repl-restore -o jsonpath='{.spec.storageClassName}')
[ "$restore_class" = stornas-replicated ] || die "restore landed on class $restore_class"
kc -n stornas-system exec repl-restore-consumer -- grep -q during-failover /data/marker \
	|| die "restored volume misses the snapshot content"

log "UI shows the failed-over placements in a real browser"
ui_phase repl-pages

# Failure matrix: 2-node split brain. Isolated bridge ports cut only
# VM-to-VM traffic, the host still reaches both. Divergence needs a
# primary on each side, so the stale side is force-promoted: the operator
# error the pick-survivor flow exists for. Quorum is off with two
# replicas, so DRBD detects the divergence on reconnect and refuses to
# merge; the API discards the loser and resyncs from the survivor.
log "splitting the two nodes: writes diverge, pick-survivor resolves"
REPL_RES=$(kc -n stornas-system get pvc repl-test -o jsonpath='{.spec.volumeName}')
kc -n stornas-system exec repl-consumer -- sh -c 'echo before-split >> /data/marker && sync'
# Two mechanisms because runners differ: port isolation cuts plain
# bridge forwarding, the physdev drops cover bridges rerouted through
# iptables by br_netfilter (docker loads it on GH runners).
partition() { # partition <on|off>
	local act=-I
	[ "$1" = off ] && act=-D
	sudo bridge link set dev tap-stornas1 isolated "$1"
	sudo bridge link set dev tap-stornas2 isolated "$1"
	sudo iptables $act FORWARD -m physdev --physdev-in tap-stornas1 --physdev-out tap-stornas2 -j DROP 2>/dev/null || true
	sudo iptables $act FORWARD -m physdev --physdev-in tap-stornas2 --physdev-out tap-stornas1 -j DROP 2>/dev/null || true
}
partition on
# The cut itself is asserted first: a silently ineffective partition
# voids every assertion after it.
partition_effective() { ! v1 ping -c 1 -W 1 "$IP2"; }
retry 60 "VM to VM traffic cut" partition_effective
# drbdsetup, never drbdadm: LINSTOR keeps its res files out of
# /etc/drbd.d, so only kernel-direct commands see these resources.
# Both sides must see the cut: DRBD rides out a dead TCP session until
# its ping cycle expires, and the force-promote below is refused while
# node2 still believes in a connected primary peer.
peer_lost() { v1 sh -c "drbdsetup status $REPL_RES | grep -q Connecting"; }
retry 300 "DRBD peer connection lost" peer_lost
peer_lost_node2() { v2 sh -c "drbdsetup status $REPL_RES | grep -q Connecting"; }
retry 120 "node2 sees the partition" peer_lost_node2
kc -n stornas-system exec repl-consumer -- sh -c 'echo during-split >> /data/marker && sync' \
	|| die "IO blocked on the primary during the partition"
# The accident under test: an operator force-promotes the stale side.
v2 sh -c "minor=\$(drbdsetup status $REPL_RES --verbose | sed -n 's/.*minor:\([0-9]*\).*/\1/p' | head -1) \
	&& drbdsetup primary --force $REPL_RES \
	&& mount /dev/drbd\$minor /mnt \
	&& echo divergent > /mnt/divergent && umount /mnt \
	&& drbdsetup secondary $REPL_RES" \
	|| die "could not diverge the stale side"

log "healing the partition: DRBD must refuse to merge"
partition off
split_detected() {
	v1 sh -c "dmesg | grep -qi split-brain" || v2 sh -c "dmesg | grep -qi split-brain"
}
retry 300 "split brain detected" split_detected
retry 600 "both nodes Ready after heal" both_ready

log "pick-survivor through the UI resolve dialog"
ui_phase split-brain
retry 900 "replicas resynced after pick-survivor" resynced
kc -n stornas-system exec repl-consumer -- cat /data/marker | grep -q during-split \
	|| die "survivor lost its writes"
log "ok: survivor kept its writes, loser was discarded"

# Failure matrix: LINSTOR controller down. The piraeus operator would
# undo the scale-down, so it goes first.
log "LINSTOR controller down: IO continues, provisioning blocks"
kc -n piraeus-datastore scale deploy piraeus-operator-controller-manager --replicas=0
kc -n piraeus-datastore scale deploy linstor-controller --replicas=0
controller_up() { kc -n piraeus-datastore get pods -l app.kubernetes.io/component=linstor-controller --no-headers 2>/dev/null | grep -q Running; }
controller_gone() { ! controller_up; }
retry 120 "controller stopped" controller_gone
kc -n stornas-system exec repl-consumer -- sh -c 'echo no-controller >> /data/marker && sync' \
	|| die "IO blocked without the LINSTOR controller"
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: blocked
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
  name: blocked-consumer
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
        claimName: blocked
EOF
sleep 45
[ "$(kc -n stornas-system get pvc blocked -o jsonpath='{.status.phase}')" = Pending ] \
	|| die "PVC bound while the controller was down"
log "ok: IO continued, provisioning blocked"

log "controller returns: blocked PVC must bind"
kc -n piraeus-datastore scale deploy piraeus-operator-controller-manager --replicas=1
kc -n piraeus-datastore scale deploy linstor-controller --replicas=1
retry 300 "controller Running again" controller_up
blocked_bound() { kc -n stornas-system get pvc blocked -o jsonpath='{.status.phase}' | grep -q Bound; }
retry 300 "blocked PVC Bound" blocked_bound

# Failure matrix: master dies, workers alive. The worker's data plane
# must keep serving with the whole control plane gone, and placement
# must not have moved when the master returns.
VIP2=$NET.61
log "master-death setup: a second target and share active on node2"
kc apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: lun1
  namespace: stornas-system
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  storageClassName: stornas-replicated
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: share1
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
  name: steer-lun1
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node2
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
      volumeDevices:
        - name: v
          devicePath: /dev/steer
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: lun1
---
apiVersion: v1
kind: Pod
metadata:
  name: steer-share1
  namespace: stornas-system
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: node2
  serviceAccountName: stornas-agent
  containers:
    - name: c
      image: ghcr.io/epheo/stornas:latest
      imagePullPolicy: Never
      command: [sleep, "7200"]
      securityContext:
        runAsUser: 0
      volumeMounts:
        - name: v
          mountPath: /data
  volumes:
    - name: v
      persistentVolumeClaim:
        claimName: share1
EOF
lun1_steered() { steered lun1; }
share1_steered() { steered share1; }
retry 600 "lun1 primary on node2" lun1_steered
retry 600 "share1 primary on node2" share1_steered
retry 60 "appliance API login" api_login
api POST /targets "{\"name\":\"md\",\"vip\":\"$VIP2/24\",\"luns\":[{\"id\":0,\"claim\":\"lun1\"}],\"initiators\":[\"$INI\"]}" \
	>/dev/null || die "target md create through the API failed"
api POST /shares "{\"name\":\"md\",\"claim\":\"share1\",\"nfsClients\":[\"$NET.0/24(rw,no_root_squash)\"]}" \
	>/dev/null || die "share md create through the API failed"
target_md_placed() { kc -n stornas-system get target md -o jsonpath='{.status.activeNode}' | grep -qx node2; }
share_md_placed() { kc -n stornas-system get share md -o jsonpath='{.status.node}' | grep -qx node2; }
retry 120 "target md placed on node2" target_md_placed
retry 120 "share md placed on node2" share_md_placed
kc -n stornas-system delete pod steer-lun1 steer-share1 --wait
target_md_on_node2() { kc -n stornas-system get target md -o jsonpath='{.status.activeNode} {.status.state}' | grep -qx "node2 Exported"; }
share_md_on_node2() { kc -n stornas-system get share md -o jsonpath='{.status.node} {.status.state}' | grep -qx "node2 Exported"; }
retry 300 "target md exported from node2" target_md_on_node2
retry 300 "share md exported from node2" share_md_on_node2
vip2_on_node2() { v2 sh -c "ip -j addr show to $VIP2 | grep -q ifname"; }
retry 120 "VIP2 held by node2" vip2_on_node2

log "killing node1: the worker data plane must not notice"
sudo kill "$(sudo cat "$WORKDIR/qemu1.pid")"
sleep 10
# No apiserver: every assert rides ssh to node2, self-clienting its own
# exports through the VIP.
md_nfs_io() {
	v2 sh -c "umount /mnt/md-e2e 2>/dev/null; mkdir -p /mnt/md-e2e && mount -t nfs -o nfsvers=4.2 $IP2:/stornas-system-md /mnt/md-e2e && echo master-down > /mnt/md-e2e/probe && sync && grep -q master-down /mnt/md-e2e/probe && umount /mnt/md-e2e"
}
retry 120 "NFS IO on node2 with the master dead" md_nfs_io
TIQN2=iqn.2026-08.io.stornas:md
LUN2_DEV="/dev/disk/by-path/ip-$VIP2:3260-iscsi-$TIQN2-lun-0"
md_iscsi_io() {
	v2 sh -c "iscsiadm -m discovery -t sendtargets -p $VIP2 >/dev/null \
		&& { iscsiadm -m node -T $TIQN2 -p $VIP2:3260 --login >/dev/null 2>&1 || true; } \
		&& test -b $LUN2_DEV \
		&& dd if=/dev/urandom of=/tmp/md-probe bs=512 count=1 2>/dev/null \
		&& dd if=/tmp/md-probe of=$LUN2_DEV bs=512 count=1 oflag=direct 2>/dev/null \
		&& dd if=$LUN2_DEV bs=512 count=1 iflag=direct 2>/dev/null | cmp - /tmp/md-probe"
}
retry 120 "iSCSI IO on node2 with the master dead" md_iscsi_io
v2 iscsiadm -m node -T "$TIQN2" -p "$VIP2:3260" --logout || true

log "restarting node1: control plane returns, placement must not move"
boot_vm 1 "$DISK1" STORNAS1 "$MAC1" tap-stornas1
retry 600 "node1 ssh back" v1 true
retry 900 "both nodes Ready after master return" both_ready
retry 300 "target md still exported from node2" target_md_on_node2
retry 300 "share md still exported from node2" share_md_on_node2
retry 900 "replicas resynced after master return" resynced

# Restart resilience: the export serves from the kernel, not the agent;
# an agent restart must be invisible to clients and to placement.
log "agent restart under live exports on node2"
AGENT2=$(kc -n stornas-system get pods -l app.kubernetes.io/name=stornas-agent -o wide --no-headers | awk '$7=="node2" {print $1}')
[ -n "$AGENT2" ] || die "no agent pod found on node2"
kc -n stornas-system delete pod "$AGENT2" --wait=false
retry 120 "NFS IO with node2's agent gone" md_nfs_io
agents_running() { [ "$(kc -n stornas-system get pods -l app.kubernetes.io/name=stornas-agent --no-headers | grep -c Running)" -eq 2 ]; }
retry 300 "agent back" agents_running
retry 120 "share md stayed exported through the restart" share_md_on_node2
retry 120 "target md stayed exported through the restart" target_md_on_node2

# Deletion is a feature: the export, VIP, and mount must leave the host,
# not just the API. The target path exercises the teardown finalizer
# (the spec carries the VIP, so it must outlive the agent's teardown).
log "target delete through the UI sheds the export and the VIP"
ui_phase delete-target
target_gone() { ! kc -n stornas-system get target failover >/dev/null 2>&1; }
retry 180 "target CR gone (finalizer released)" target_gone
node1_shed_export() { ! v1 sh -c 'targetcli ls /iscsi 1 2>/dev/null | grep -q stornas:failover'; }
node1_shed_vip() { ! v1 sh -c "ip -j addr show to $VIP | grep -q ifname"; }
retry 120 "node1 shed the deleted export" node1_shed_export
retry 60 "node1 shed the deleted target's VIP" node1_shed_vip

log "share delete through the UI unexports and unmounts"
ui_phase delete-share
share_unexported() { ! v1 sh -c 'exportfs | grep -q stornas-system-failover'; }
share_unmounted() { ! v1 findmnt /var/lib/stornas/shares/stornas-system-failover; }
retry 120 "NFS export gone from node1" share_unexported
retry 60 "share mount gone from node1" share_unmounted

log "pool delete refused while volumes remain, in the dialog and the API"
ui_phase delete-pool-refused
retry 60 "appliance API login" api_login
code=$(curl -s -m 15 -o /dev/null -w '%{http_code}' -b "$WORKDIR/cookies" -X DELETE \
	"http://$IP1:30080/api/v1/pools/pool-node1")
[ "$code" = 409 ] || die "pool delete got $code, want 409"
kc get storagepool pool-node1 >/dev/null 2>&1 || die "guarded pool was deleted anyway"

# Replicated lifecycle closes where it opened: deletion must reach the
# hosts, dropping the DRBD resource on both, not just the PVC.
log "replicated snapshot and volumes deleted through the UI dialogs"
TARGET_SNAP=repl-snap ui_phase delete-snapshot
repl_snap_gone() { ! kc -n stornas-system get volumesnapshot repl-snap >/dev/null 2>&1; }
retry 120 "replicated snapshot gone" repl_snap_gone
# pvc-protection holds claimed volumes; the consumers go first.
kc -n stornas-system delete pod repl-restore-consumer repl-consumer --wait
TARGET_VOL=repl-restore ui_phase delete-volume
TARGET_VOL=repl-test ui_phase delete-volume
pvc_gone() { ! kc -n stornas-system get pvc "$1" >/dev/null 2>&1; }
retry 180 "repl-restore PVC gone" pvc_gone repl-restore
retry 180 "repl-test PVC gone" pvc_gone repl-test
res_gone() { ! $1 sh -c "drbdsetup status $REPL_RES" >/dev/null 2>&1; }
retry 180 "DRBD resource gone from node1" res_gone v1
retry 180 "DRBD resource gone from node2" res_gone v2

# Run the packaged check scripts directly (distro multinode-test shape):
# exactly what greenboot executes, and the stornas check must pass on
# the controller (full stack) and short-circuit on the worker.
log "greenboot health checks green on both roles"
greenboot_checks() { # greenboot_checks <vssh-fn> <label>
	$1 sh -c 'rc=0; for s in /usr/lib/greenboot/check/required.d/*.sh /etc/greenboot/check/required.d/*.sh; do
		[ -e "$s" ] || continue
		echo "== $s"
		if ! "$s"; then echo "CHECK FAILED: $s"; rc=1; fi
	done; exit $rc' || die "greenboot checks red on the $2"
}
greenboot_checks v1 controller
greenboot_checks v2 worker

log "REPLICATION AND FAILOVER TEST PASSED"
