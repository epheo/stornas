#!/usr/bin/env bash
# Shared by boot-test.sh, replication-test.sh, smoke-test.sh.
# Sourced only: no set -e here, the sourcing script's set -euo pipefail
# already covers everything this file defines.

log() { printf '\n== %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

retry() { # retry <seconds> <description> <cmd...>; cmd runs in this shell
	local deadline=$(( $(date +%s) + $1 )) desc=$2
	printf 'waiting up to %ss for: %s\n' "$1" "$desc"
	shift 2
	until "$@" >/dev/null 2>&1; do
		[ "$(date +%s)" -gt "$deadline" ] && die "timed out waiting for: $desc"
		sleep 5
	done
	log "ok: $desc"
}

# Sync the image into rootful storage by ID, not mere existence: a stale
# rootful copy would boot silently and old bugs would resurface.
sync_rootful_image() { # sync_rootful_image <image> <podman-cmd>
	local image=$1 podman=$2 want have
	if [ "$podman" != podman ] && podman image exists "$image"; then
		want=$(podman image inspect -f '{{.Id}}' "$image")
		have=$($podman image inspect -f '{{.Id}}' "$image" 2>/dev/null || true)
		if [ "$want" != "$have" ]; then
			log "syncing $image into rootful podman storage"
			podman save "$image" | $podman load
		fi
	else
		$podman image exists "$image" || die "image $image not found in $podman storage"
	fi
}
