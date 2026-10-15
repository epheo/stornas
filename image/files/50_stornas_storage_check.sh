#!/bin/bash
# Required greenboot check for the stornas storage plane, replacing the
# microshift-topolvm check erased in the Containerfile. Gates on workloads
# with static names only.
set -euo pipefail

# shellcheck disable=SC1091
source /usr/share/microshift/functions/greenboot.sh

if [ "$(id -u)" -ne 0 ] ; then
    echo "The '${SCRIPT_NAME}' script must be run with the 'root' user privileges"
    exit 1
fi

exit_if_fail_marker_exists

echo "STARTED"

# A worker holds only kubelet credentials and cannot query deployments;
# the packaged role-aware healthcheck already gates on its node being
# Ready, and the storage workloads live on the controller.
if [ "$(microshift-profile 2>/dev/null)" = "worker" ]; then
    echo "worker profile: storage plane gated on the controller"
    exit 0
fi

WAIT_TIMEOUT_SECS=$(get_wait_timeout)

if ! microshift healthcheck \
        -v=2 --timeout="${WAIT_TIMEOUT_SECS}s" \
        --custom '{
            "r-drbd-system": {
                "deployments": ["r-drbd-controller"],
                "daemonsets": ["r-drbd-agent"]
            },
            "stornas-system": {
                "deployments": ["stornas-operator", "stornas-server"],
                "daemonsets": ["stornas-agent"]
            }
        }'; then
    create_fail_marker_and_exit
fi
