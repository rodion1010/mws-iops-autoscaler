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

VM_REF="compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}"
POLL_SECONDS="${TEARDOWN_POLL_SECONDS:-5}"
POLL_ATTEMPTS="${TEARDOWN_POLL_ATTEMPTS:-24}"

# The VM goes first: its own dependent disks go with it, and a separately created disk cannot be
# deleted while attached. The delete call can fail while the platform goes on deleting the VM, so
# its result is only logged; what counts is the VM being gone within the next 2 minutes.
delete mws compute virtual-machine delete "$VM_REF" --wait-timeout 3m ||
  echo "teardown: VM delete for ${VM_NAME} reported an error; waiting for the VM to disappear" >&2
vm_gone=false
for _ in $(seq 1 "$POLL_ATTEMPTS"); do
  if out=$(mws compute virtual-machine get "$VM_REF" 2>&1); then
    sleep "$POLL_SECONDS"
  elif grep -q 'NOT_FOUND' <<<"$out"; then
    vm_gone=true
    break
  else
    echo "$out" >&2
    sleep "$POLL_SECONDS"
  fi
done
if [ "$vm_gone" != true ]; then
  # Its disks are still attached, so deleting them would only fail; the sweeper retries later.
  echo "teardown: VM ${VM_NAME} still exists; its disks were not deleted" >&2
  exit 1
fi

# The disks are independent of each other once the VM is gone and run in parallel, each bounded so
# the phase's worst case is one disk delete's wait. The callers' teardown timeouts cover the VM
# delete's 3m, the 2m wait for the VM to disappear and this 2m.
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
