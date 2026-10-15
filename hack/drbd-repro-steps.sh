# Sourced by replication-test.sh (REPRO_SCRIPT) after the three-node
# bring-up; v1/v2/kc/volume_replicas/retry/log are in scope. Not a gate:
# it always exits 0 and its product is the VERDICT and GI lines plus the DRBD
# dumps, answering why Immediate-created resources dodge split-brain
# detection (memory: immediate-flip-blocked).

log "repro: one host-declared WFFC volume, one Immediate volume"
kc apply -f - <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: repro-imm
provisioner: r-drbd.io
parameters:
  tier: default
  replicas: "2"
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
	[ "$(volume_replicas "$1" | awk '$2 == "diskful" && $3 == "UpToDate"' | wc -l)" -ge 2 ]
}
retry 600 "$RES_W two UpToDate replicas" uptodate "$RES_W"
retry 600 "$RES_I two UpToDate replicas" uptodate "$RES_I"

dump() { # dump <res> - birth certificate and live state, both nodes
	log "repro: dump $1"
	echo "--- r-drbd volume"; kc get volume "$1" -o yaml 2>&1 || true
	for n in v1 v2; do
		echo "--- $n drbdsetup show"; $n timeout 30 drbdsetup show "$1" 2>&1 || true
		echo "--- $n drbdsetup status --verbose"; $n timeout 30 drbdsetup status "$1" --verbose --statistics 2>&1 || true
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
	if [ "$1" = off ]; then
		sudo nft delete table bridge stornas_cut
		return
	fi
	sudo nft -f - <<'NFT'
table bridge stornas_cut {
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname "tap-stornas2" drop
		oifname "tap-stornas2" drop
	}
}
NFT
}

# get-gi prints current:bitmap:history1:history2:flags as seen toward the
# peer. A non-zero bitmap UUID while connected and in sync is the stale
# slot that stops the next lone write from minting a generation.
gi() { # gi <node-fn> <res>
	local peer
	# node3 is the diskless tiebreaker; the data peer is the other one.
	peer=$("$1" sh -c "drbdsetup status $2 --verbose | grep -v '^  node3 ' | sed -n 's/^  [^ ]* node-id:\([0-9]*\).*/\1/p' | head -1")
	"$1" timeout 30 drbdsetup get-gi "$2" "$peer" 0 2>&1 || echo unknown
}
report_gi() { # report_gi <when> <res>
	echo "GI $2 $1: node1 $(gi v1 "$2") node2 $(gi v2 "$2")"
}

diverge() { # diverge <res>
	local res=$1 m1 m2
	m1=$(minor_of v1 "$res"); m2=$(minor_of v2 "$res")
	log "repro: diverging $res (minors node1=$m1 node2=$m2)"
	# The partition cuts every resource, so an earlier cycle can reset
	# this one's slots; record the state this cycle really starts from.
	report_gi before-divergence "$res"
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
	report_gi after-divergence "$res"
	v1 timeout 30 drbdsetup secondary "$res" 2>/dev/null || true
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
	v1 timeout 30 drbdsetup secondary "$res" 2>/dev/null || true
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
report_gi after-history "$RES_W"
report_gi after-history "$RES_I"
dump "$RES_W"
dump "$RES_I"

diverge "$RES_W"
diverge "$RES_I"

log "repro: post-divergence state"
dump "$RES_W"
dump "$RES_I"
log "repro: done, see VERDICT lines"
