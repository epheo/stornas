#!/bin/bash
# Required greenboot check for the stornas storage plane, replacing the
# microshift-topolvm check erased in the Containerfile. Gates on workloads
# with static names only; LINSTOR satellites are per-node objects the
# piraeus operator names dynamically, so they are covered indirectly by
# linstor-controller needing a working cluster.
set -euo pipefail

# shellcheck disable=SC1091
source /usr/share/microshift/functions/greenboot.sh

if [ "$(id -u)" -ne 0 ] ; then
    echo "The '${SCRIPT_NAME}' script must be run with the 'root' user privileges"
    exit 1
fi

exit_if_fail_marker_exists

echo "STARTED"

WAIT_TIMEOUT_SECS=$(get_wait_timeout)

if ! microshift healthcheck \
        -v=2 --timeout="${WAIT_TIMEOUT_SECS}s" \
        --custom '{
            "piraeus-datastore": {
                "deployments": ["piraeus-operator-controller-manager", "linstor-controller"]
            },
            "stornas-system": {
                "deployments": ["stornas-operator", "stornas-server"],
                "daemonsets": ["stornas-agent"]
            }
        }'; then
    create_fail_marker_and_exit
fi
