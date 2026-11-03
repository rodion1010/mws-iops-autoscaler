#!/usr/bin/env bash
# Deletes e2e-full VMs and disks that outlived their run (cancelled job, lost runner).
#
# Ownership is the run-scoped name provision.sh gives its resources: e2e-full-<numeric run id> for the
# VM and a fixed suffix per disk. Age comes from the API's metadata.createTime, so the sweep needs
# nothing written by the test itself. Anything else is never touched; a name that looks like ours but
# does not match exactly, or an unreadable age, is reported and fails the run instead of being guessed.
#
# Dry-run unless SWEEP_APPLY=true. Output: one tab-separated "ACTION kind name age_hours" line each.
set -uo pipefail

: "${MWS_PROJECT:?}"
TTL_HOURS="${SWEEP_TTL_HOURS:-3}"
NOW="${SWEEP_NOW:-$(date +%s)}"
CUTOFF=$((NOW - TTL_HOURS * 3600))
VM_RE='^e2e-full-[0-9]+$'
DISK_RE='^e2e-full-[0-9]+-(boot|data|noname|churn)$'

status=0

# Prints "<name>\t<createTime>" for every resource of a kind. Any list failure or unfamiliar response
# shape fails closed so a partial listing never drives a delete.
list_resources() {
  local kind=$1 token="" page
  while :; do
    page=$(mws compute "$kind" list --project "$MWS_PROJECT" ${token:+--page-token "$token"} -f json) || return 1
    jq -e 'keys - ["items", "nextPageToken"] == []' <<<"$page" >/dev/null || return 1
    jq -r '.items[]? | [(.metadata.id | split("/") | last), (.metadata.createTime // "")] | @tsv' <<<"$page" || return 1
    token=$(jq -r '.nextPageToken // empty' <<<"$page") || return 1
    [ -z "$token" ] && return 0
  done
}

# sweep <kind> <resource path segment> <name regex> <extra delete flag, or empty> <wait timeout>
sweep() {
  local kind=$1 segment=$2 re=$3 purge=$4 wait=$5 listing name created epoch age
  if ! listing=$(list_resources "$kind"); then
    echo "sweep: cannot list ${kind}s; deleting none of them" >&2
    status=1
    return
  fi
  while IFS=$'\t' read -r name created; do
    [[ $name == e2e-full-* ]] || continue
    if ! [[ $name =~ $re ]]; then
      printf 'SKIP-AMBIGUOUS-NAME\t%s\t%s\t-\n' "$kind" "$name"
      status=1
      continue
    fi
    if ! epoch=$(jq -nr --arg t "$created" '$t | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601' 2>/dev/null); then
      printf 'SKIP-AMBIGUOUS-AGE\t%s\t%s\t-\n' "$kind" "$name"
      status=1
      continue
    fi
    age=$(((NOW - epoch) / 3600))
    if [ "$epoch" -gt "$CUTOFF" ]; then
      printf 'KEEP-YOUNG\t%s\t%s\t%s\n' "$kind" "$name" "$age"
    elif [ "${SWEEP_APPLY:-false}" != true ]; then
      printf 'WOULD-DELETE\t%s\t%s\t%s\n' "$kind" "$name" "$age"
    elif mws compute "$kind" delete "compute/projects/${MWS_PROJECT}/${segment}/${name}" $purge --wait-timeout "$wait" </dev/null; then
      printf 'DELETED\t%s\t%s\t%s\n' "$kind" "$name" "$age"
    else
      printf 'DELETE-FAILED\t%s\t%s\t%s\n' "$kind" "$name" "$age"
      status=1
    fi
  done <<<"$listing"
}

# VMs first: a disk cannot be deleted while the VM that owns it still exists.
sweep virtual-machine virtualMachines "$VM_RE" "" 5m
sweep disk disks "$DISK_RE" --purge 2m

exit "$status"
