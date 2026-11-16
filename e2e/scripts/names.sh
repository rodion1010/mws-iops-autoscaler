#!/usr/bin/env bash
# Prints the run-scoped names provision.sh creates and teardown.sh deletes, so both agree.
#
# Usage: names.sh vm|disks. "disks" lists the separately created disks, one per line; a smoke VM's
# dependent data disk is owned by the VM and has no name of ours.
# full:  e2e-full-<run id>
# smoke: e2e-smoke-<run id>-<run attempt>-s<scenario index>, one VM per scenario and attempt
set -euo pipefail

: "${E2E_RUN_ID:?}"
number() { [[ $2 =~ ^[0-9]+$ ]] || { echo "names.sh: $1 must be a number, got '$2'" >&2; exit 2; }; }
number E2E_RUN_ID "$E2E_RUN_ID"

case "${E2E_TIER:-full}" in
full)
  vm="e2e-full-${E2E_RUN_ID}"
  disks=(boot data noname churn)
  ;;
smoke)
  : "${E2E_RUN_ATTEMPT:?}" "${E2E_SCENARIO_ID:?}"
  number E2E_RUN_ATTEMPT "$E2E_RUN_ATTEMPT"
  number E2E_SCENARIO_ID "$E2E_SCENARIO_ID"
  vm="e2e-smoke-${E2E_RUN_ID}-${E2E_RUN_ATTEMPT}-s${E2E_SCENARIO_ID}"
  disks=(boot indep)
  ;;
*)
  echo "names.sh: E2E_TIER must be full or smoke, got '${E2E_TIER}'" >&2
  exit 2
  ;;
esac

case "${1:-}" in
vm) echo "$vm" ;;
disks) for d in "${disks[@]}"; do echo "${vm}-${d}"; done ;;
*) echo "usage: names.sh vm|disks" >&2; exit 2 ;;
esac
