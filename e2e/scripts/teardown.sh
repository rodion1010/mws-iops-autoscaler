#!/usr/bin/env bash
# Tears down the run-scoped VM and disks created by provision.sh.
set -uo pipefail

: "${MWS_PROJECT:?}" "${E2E_RUN_ID:?}"

VM_NAME="e2e-full-${E2E_RUN_ID}"
BOOT_DISK="${VM_NAME}-boot"
DATA_DISK="${VM_NAME}-data"
NONAME_DISK="${VM_NAME}-noname"
CHURN_DISK="${VM_NAME}-churn"

# Attempt every deletion and report an aggregate status.
status=0

mws compute virtual-machine delete "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" --wait-timeout 5m || {
  echo "teardown: failed to delete VM ${VM_NAME}" >&2
  status=1
}

# The four disks are independent of each other once the VM itself is gone
# (CHURN_DISK may or may not have been attached, but --purge deletes it
# either way) and run in parallel, each explicitly bounded so the phase's
# own worst case is one disk delete's wait, not four — e2e/lifecycle_test.go's
# teardownTimeout is the sum of this --wait-timeout and the VM delete's above.
disk_names=("$DATA_DISK" "$NONAME_DISK" "$CHURN_DISK" "$BOOT_DISK")
disk_pids=()
for disk in "${disk_names[@]}"; do
  mws compute disk delete "compute/projects/${MWS_PROJECT}/disks/${disk}" --purge --wait-timeout 2m &
  disk_pids+=("$!")
done
for i in "${!disk_pids[@]}"; do
  if ! wait "${disk_pids[$i]}"; then
    echo "teardown: failed to delete disk ${disk_names[$i]}" >&2
    status=1
  fi
done

exit "$status"
