# Sourced by replication-test.sh (REPRO_SCRIPT) after the two-node
# bring-up; v1/v2/kc/linstor_cmd/retry/log are in scope. Not a gate:
# it always exits 0 and its product is the VERDICT lines plus the DRBD
# dumps, answering why Immediate-created resources dodge split-brain
# detection (memory: immediate-flip-blocked).

log "repro: one binder-declared WFFC volume, one Immediate volume"
kc apply -f - <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: repro-imm
provisioner: linstor.csi.linbit.com
parameters:
  linstor.csi.linbit.com/storagePool: stornas
  linstor.csi.linbit.com/layerList: drbd storage
  linstor.csi.linbit.com/placementCount: "2"
  property.linstor.csi.linbit.com/DrbdOptions/auto-quorum: suspend-io
  property.linstor.csi.linbit.com/DrbdOptions/Net/protocol: C
  csi.storage.k8s.io/fstype: xfs
volumeBindingMode: Immediate
allowVolumeExpansion: true
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: repro-wffc
  namespace: stornas-system
  annotations:
    storage.stornas.io/consumer: host
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: stornas-replicated
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: repro-imm
  namespace: stornas-system
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: repro-imm
  resources:
    requests:
      storage: 1Gi
EOF

bound() { kc -n stornas-system get pvc "$1" -o jsonpath='{.spec.volumeName}' | grep -q pvc-; }
retry 600 "repro-wffc Bound" bound repro-wffc
retry 600 "repro-imm Bound" bound repro-imm
RES_W=$(kc -n stornas-system get pvc repro-wffc -o jsonpath='{.spec.volumeName}')
RES_I=$(kc -n stornas-system get pvc repro-imm -o jsonpath='{.spec.volumeName}')
log "repro: WFFC resource $RES_W, Immediate resource $RES_I"

uptodate() {
	[ "$(linstor_cmd -m resource list -r "$1" 2>/dev/null | grep -o UpToDate | wc -l)" -ge 2 ]
}
retry 600 "$RES_W two UpToDate replicas" uptodate "$RES_W"
retry 600 "$RES_I two UpToDate replicas" uptodate "$RES_I"

dump() { # dump <res> - birth certificate and live state, both nodes
	log "repro: dump $1"
	echo "--- linstor rd properties"; linstor_cmd resource-definition list-properties "$1" 2>&1 || true
	echo "--- linstor resource list"; linstor_cmd resource list -r "$1" 2>&1 || true
	for n in v1 v2; do
		echo "--- $n drbdsetup show"; $n drbdsetup show "$1" 2>&1 || true
		echo "--- $n drbdsetup status --verbose"; $n drbdsetup status "$1" --verbose --statistics 2>&1 || true
		echo "--- $n dmesg for $1 (uuid/handshake lines)"
		$n sh -c "dmesg | grep '$1' | grep -Ei 'uuid|handshake|bitmap|sync|split' | tail -30" 2>&1 || true
	done
}
dump "$RES_W"
dump "$RES_I"

minor_of() { # minor_of <node-fn> <res>
	"$1" sh -c "drbdsetup status $2 --verbose | sed -n 's/.*minor:\([0-9]*\).*/\1/p' | head -1"
}

# The gate's accident, replayed per resource: node1 writes as primary
# through the partition, node2 force-promotes and writes the stale side,
# the partition heals. Serial cycles: the idle resource reconnects clean
# so the earlier cycle cannot contaminate the later one.
partition() { # partition <on|off> - copy of the gate's helper, defined below the hook
	local act=-I
	[ "$1" = off ] && act=-D
	sudo bridge link set dev tap-stornas1 isolated "$1"
	sudo bridge link set dev tap-stornas2 isolated "$1"
	sudo iptables $act FORWARD -m physdev --physdev-in tap-stornas1 --physdev-out tap-stornas2 -j DROP 2>/dev/null || true
	sudo iptables $act FORWARD -m physdev --physdev-in tap-stornas2 --physdev-out tap-stornas1 -j DROP 2>/dev/null || true
}

diverge() { # diverge <res>
	local res=$1 m1 m2
	m1=$(minor_of v1 "$res"); m2=$(minor_of v2 "$res")
	log "repro: diverging $res (minors node1=$m1 node2=$m2)"
	v1 drbdsetup primary "$res" || { echo "VERDICT $res: node1 promote failed"; return; }
	v1 sh -c "dd if=/dev/urandom of=/dev/drbd$m1 bs=4k count=16 oflag=direct conv=fsync" 2>/dev/null || echo "NOTE $res: node1 write failed"
	partition on
	cut() { ! v1 ping -c 1 -W 1 "$IP2"; }
	retry 60 "traffic cut for $res" cut
	lost() { v1 sh -c "drbdsetup status $res | grep -q Connecting"; }
	retry 120 "peer lost for $res" lost
	v1 sh -c "dd if=/dev/urandom of=/dev/drbd$m1 bs=4k count=16 seek=64 oflag=direct conv=fsync" 2>/dev/null || echo "NOTE $res: node1 divergent write failed"
	v2 sh -c "drbdsetup primary --force $res \
		&& dd if=/dev/urandom of=/dev/drbd$m2 bs=4k count=16 seek=128 oflag=direct conv=fsync \
		&& drbdsetup secondary $res" 2>/dev/null \
		|| echo "NOTE $res: stale-side divergence incomplete"
	partition off
	# Observe with a plain loop, never die: the asymmetry between the two
	# resources IS the result. 120s covers reconnect plus the handshake.
	local t=0 hit=1
	while [ "$t" -lt 120 ]; do
		if v1 sh -c "dmesg | grep '$res' | grep -qi split-brain" \
			|| v2 sh -c "dmesg | grep '$res' | grep -qi split-brain"; then
			hit=0
			break
		fi
		sleep 5
		t=$((t + 5))
	done
	if [ "$hit" -eq 0 ]; then
		echo "VERDICT $res: SPLIT-BRAIN DETECTED after ${t}s"
	else
		echo "VERDICT $res: NO DETECTION after 120s"
	fi
	v1 drbdsetup secondary "$res" 2>/dev/null || true
}

# Round 2: fresh resources detect split-brain on both paths (run
# 32199988923), so creation alone is innocent. The failing gate runs
# had history before the accident; replay its UUID-relevant parts:
# plain IO, a peer-loss resync (current rotates, bitmap fills), and a
# CSI resize.
history() { # history <res> <pvc>
	local res=$1 pvc=$2 m1
	m1=$(minor_of v1 "$res")
	log "repro: history for $res: IO, peer-loss resync, resize"
	v1 drbdsetup primary "$res" 2>/dev/null || echo "NOTE $res: history promote failed"
	v1 sh -c "dd if=/dev/urandom of=/dev/drbd$m1 bs=4k count=32 oflag=direct conv=fsync" 2>/dev/null \
		|| echo "NOTE $res: history write failed"
	partition on
	hcut() { ! v1 ping -c 1 -W 1 "$IP2"; }
	retry 60 "history cut for $res" hcut
	hlost() { v1 sh -c "drbdsetup status $res | grep -q Connecting"; }
	retry 120 "history peer lost for $res" hlost
	v1 sh -c "dd if=/dev/urandom of=/dev/drbd$m1 bs=4k count=32 seek=32 oflag=direct conv=fsync" 2>/dev/null \
		|| echo "NOTE $res: lone-primary write failed"
	partition off
	retry 300 "$res resynced UpToDate" uptodate "$res"
	v1 drbdsetup secondary "$res" 2>/dev/null || true
	kc -n stornas-system patch pvc "$pvc" --type merge \
		-p '{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}' \
		|| echo "NOTE $res: resize request refused"
	# sysfs works on a Secondary; opening the device would not.
	local t=0
	while [ "$t" -lt 300 ]; do
		[ "$(v1 cat "/sys/block/drbd$m1/size" 2>/dev/null || echo 0)" -ge 4194304 ] && break
		sleep 5
		t=$((t + 5))
	done
	if [ "$t" -ge 300 ]; then
		echo "NOTE $res: resize did not land in 300s, continuing without it"
	else
		log "ok: $res resized to 2Gi"
	fi
}

history "$RES_W" repro-wffc
history "$RES_I" repro-imm
log "repro: post-history state"
dump "$RES_W"
dump "$RES_I"

diverge "$RES_W"
diverge "$RES_I"

log "repro: post-divergence state"
dump "$RES_W"
dump "$RES_I"
log "repro: done, see VERDICT lines"
