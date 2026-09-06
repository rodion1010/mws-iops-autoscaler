#!/usr/bin/env bash
# Tears down the run-scoped VM and disks created by provision.sh.
set -uo pipefail

: "${MWS_PROJECT:?}" "${E2E_RUN_ID:?}"

VM_NAME="e2e-full-${E2E_RUN_ID}"
BOOT_DISK="${VM_NAME}-boot"
DATA_DISK="${VM_NAME}-data"

# Attempt every deletion and report an aggregate status.
status=0

mws compute virtual-machine delete "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" || {
  echo "teardown: failed to delete VM ${VM_NAME}" >&2
  status=1
}

mws compute disk delete "compute/projects/${MWS_PROJECT}/disks/${DATA_DISK}" --purge || {
  echo "teardown: failed to delete disk ${DATA_DISK}" >&2
  status=1
}

mws compute disk delete "compute/projects/${MWS_PROJECT}/disks/${BOOT_DISK}" --purge || {
  echo "teardown: failed to delete disk ${BOOT_DISK}" >&2
  status=1
}

exit "$status"
