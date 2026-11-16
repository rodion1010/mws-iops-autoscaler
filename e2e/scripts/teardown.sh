#!/usr/bin/env bash
# Tears down the run-scoped VM and disks created by provision.sh (same E2E_TIER and identity env).
# A resource that is already gone counts as deleted, so a partial provision tears down cleanly.
set -uo pipefail

: "${MWS_PROJECT:?}" "${E2E_RUN_ID:?}"

here=$(dirname "$0")
VM_NAME=$(bash "$here/names.sh" vm) || exit 2
disks_out=$(bash "$here/names.sh" disks) || exit 2
disk_names=()
while IFS= read -r d; do disk_names+=("$d"); done <<<"$disks_out"

# Runs an mws delete; NOT_FOUND is success.
delete() {
  local out
  out=$("$@" 2>&1) && return 0
  grep -q 'NOT_FOUND' <<<"$out" && return 0
  echo "$out" >&2
  return 1
}

# Attempt every deletion and report an aggregate status.
status=0

# The VM goes first: its own dependent disks go with it, and a separately created disk cannot be
# deleted while attached.
delete mws compute virtual-machine delete "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" --wait-timeout 5m || {
  echo "teardown: failed to delete VM ${VM_NAME}" >&2
  status=1
}

# The disks are independent of each other once the VM is gone and run in parallel, each bounded so
# the phase's worst case is one disk delete's wait; the callers' teardown timeouts are the sum of
# this --wait-timeout and the VM delete's above.
disk_pids=()
for disk in "${disk_names[@]}"; do
  delete mws compute disk delete "compute/projects/${MWS_PROJECT}/disks/${disk}" --purge --wait-timeout 2m &
  disk_pids+=("$!")
done
for i in "${!disk_pids[@]}"; do
  if ! wait "${disk_pids[$i]}"; then
    echo "teardown: failed to delete disk ${disk_names[$i]}" >&2
    status=1
  fi
done

exit "$status"
