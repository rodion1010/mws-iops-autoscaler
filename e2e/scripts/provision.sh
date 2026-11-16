#!/usr/bin/env bash
# Provisions one ephemeral VM and its disks for an E2E tier (E2E_TIER: full or smoke).
#
# All environment-specific identifiers are supplied at runtime. VMs get internal addresses only and
# reach the internet through the subnet's Egress NAT; the runner shares the subnet and connects over
# SSH to the internal address. Exit status 3 means the project quota is exhausted: nothing is
# retried, and teardown.sh removes whatever was already created.
set -euo pipefail

: "${MWS_PROJECT:?}" "${MWS_ZONE:?}" "${E2E_RUN_ID:?}" "${E2E_SUBNET_ID:?}" \
	"${E2E_BOOT_IMAGE:?}" "${E2E_VM_TYPE:?}" "${E2E_AGENT_SA:?}" "${E2E_SSH_PUBLIC_KEY:?}" \
	"${E2E_SSH_USER:?}" "${E2E_SSH_KEY_PATH:?}"

E2E_TIER="${E2E_TIER:-full}"
here=$(dirname "$0")
VM_NAME=$(bash "$here/names.sh" vm)
DISKS="compute/projects/${MWS_PROJECT}/disks"

# Runs an mws create command; a quota rejection exits 3 with a plain reason instead of a generic failure.
create() {
  local out
  if ! out=$("$@" 2>&1); then
    if grep -qi 'quota' <<<"$out"; then
      echo "provision.sh: quota exhausted creating ${VM_NAME}: $(grep -i -m1 'quota' <<<"$out")" >&2
      exit 3
    fi
    echo "$out" >&2
    return 1
  fi
}

disk() { # name size [extra flags]
  create mws compute disk create "${DISKS}/$1" --zone "${MWS_ZONE}" --disk-type "compute/diskTypes/nbs-pl2" \
    --size "$2" --iops 1000 "${@:3}"
}

disk "${VM_NAME}-boot" 20GB --source-image "${E2E_BOOT_IMAGE}" --os-type LINUX
BOOT_ENTRY="{name: boot, boot: true, deviceName: bootdisk, disk: {ref: \"${DISKS}/${VM_NAME}-boot\"}}"

if [ "$E2E_TIER" = smoke ]; then
  # One dependent disk, owned by the VM through its spec, and one independent disk attached by
  # reference: every smoke scenario needs one class or both.
  DEVICE_NAME="e2edata"
  INDEPENDENT_DEVICE_NAME="e2eindep"
  disk "${VM_NAME}-indep" 10GB
  STORAGE=(
    --storage-disks "$BOOT_ENTRY"
    --storage-disks "{name: data, boot: false, deviceName: ${DEVICE_NAME}, disk: {spec: {diskType: compute/diskTypes/nbs-pl2, size: 10GB, iops: 1000}}}"
    --storage-disks "{name: indep, boot: false, deviceName: ${INDEPENDENT_DEVICE_NAME}, disk: {ref: \"${DISKS}/${VM_NAME}-indep\"}}"
  )
else
  DEVICE_NAME="e2edata"
  # The noname disk carries no deviceName, so the platform defaults its guest device to
  # mws-{alias}; this covers that default live, alongside the data disk's explicit deviceName.
  NONAME_ALIAS="e2enoname"
  # The churn disk is created unattached; scenarios attach and detach it live via
  # AttachChurnDisk/DetachChurnDisk. Its deviceName is explicit so that only the noname disk
  # exercises the mws-{alias} default and a failure is unambiguous.
  CHURN_DEVICE_NAME="e2echurn"
  for d in data noname churn; do disk "${VM_NAME}-${d}" 20GB; done
  # Echoed verbatim below: vm update --storage-disks replaces the whole list, so the Go side resends
  # these exact strings when it attaches or detaches the churn disk instead of reconstructing them.
  DATA_ENTRY="{name: data, boot: false, deviceName: ${DEVICE_NAME}, disk: {ref: \"${DISKS}/${VM_NAME}-data\"}}"
  NONAME_ENTRY="{name: ${NONAME_ALIAS}, boot: false, disk: {ref: \"${DISKS}/${VM_NAME}-noname\"}}"
  CHURN_ENTRY="{name: churn, boot: false, deviceName: ${CHURN_DEVICE_NAME}, disk: {ref: \"${DISKS}/${VM_NAME}-churn\"}}"
  STORAGE=(--storage-disks "$BOOT_ENTRY" --storage-disks "$DATA_ENTRY" --storage-disks "$NONAME_ENTRY")
fi

# Raw-device fio requires passwordless sudo for the test user.
USER_DATA=$'#cloud-config\nusers:\n  - name: '"${E2E_SSH_USER}"$'\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    shell: /bin/bash\n    ssh-authorized-keys:\n      - '"${E2E_SSH_PUBLIC_KEY}"$'\npackage_update: true\npackages:\n  - fio\n  - sysstat\n  - jq'

# --wait-timeout defaults to 5m, which VM creation in this zone can exceed. The caller's
# provisioning context still bounds the whole script.
create mws compute virtual-machine create "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" \
  --zone "${MWS_ZONE}" \
  --vm-type "${E2E_VM_TYPE}" \
  --service-account "${E2E_AGENT_SA}" \
  --wait-timeout 10m \
  --os-metadata-attributes "user-data: |
$(sed 's/^/  /' <<< "$USER_DATA")" \
  --network-interfaces "name: eth0, primary: true, addresses: [{address: {spec: {subnet: \"${E2E_SUBNET_ID}\"}}}]" \
  "${STORAGE[@]}"

SSH_HOST=""
NONAME_DEVICE_NAME=""
# The address and status.storage.disks[].deviceName may not be populated the instant creation
# returns, so re-fetch a few times.
for _ in $(seq 1 6); do
  VM_JSON=$(mws compute virtual-machine get "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" -f json)
  SSH_HOST=$(jq -r '.status.network.networkInterfaces[0].addresses[0].ipAddress // empty' <<< "$VM_JSON")
  if [ "$E2E_TIER" = full ]; then
    NONAME_DEVICE_NAME=$(jq -r --arg alias "$NONAME_ALIAS" \
      '.status.storage.disks[] | select(.name == $alias) | .deviceName // empty' <<< "$VM_JSON")
    [ -n "$SSH_HOST" ] && [ -n "$NONAME_DEVICE_NAME" ] && break
  elif [ -n "$SSH_HOST" ]; then
    break
  fi
  sleep 5
done

if [ -z "$SSH_HOST" ]; then
  echo "provision.sh: could not resolve the internal address of ${VM_NAME}" >&2
  exit 1
fi
if [ "$E2E_TIER" = full ] && [ -z "$NONAME_DEVICE_NAME" ]; then
  echo "provision.sh: could not resolve the server-assigned deviceName for ${VM_NAME}'s ${NONAME_ALIAS} disk" >&2
  exit 1
fi

SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes -i "${E2E_SSH_KEY_PATH:?}")

# Wait for SSH before checking guest readiness.
ssh_up=false
for _ in $(seq 1 30); do
  if ssh "${SSH_OPTS[@]}" "${E2E_SSH_USER}@${SSH_HOST}" true 2>/dev/null; then
    ssh_up=true
    break
  fi
  sleep 10
done
if [ "$ssh_up" != true ]; then
  echo "provision.sh: ${VM_NAME} (${SSH_HOST}) never became SSH-reachable" >&2
  exit 1
fi

# SSH can precede cloud-init completion; verify both status and fio directly.
cloud_init_out=""
for _ in $(seq 1 18); do
  cloud_init_out=$(ssh "${SSH_OPTS[@]}" "${E2E_SSH_USER}@${SSH_HOST}" "cloud-init status" 2>/dev/null || true)
  case "$cloud_init_out" in
    *"status: done"*|*"status: error"*) break ;;
  esac
  sleep 10
done
if [[ "$cloud_init_out" != *"status: done"* ]]; then
  echo "provision.sh: ${VM_NAME} (${SSH_HOST}) cloud-init did not report done: ${cloud_init_out}" >&2
  exit 1
fi

fio_ready=false
for _ in $(seq 1 6); do
  if ssh "${SSH_OPTS[@]}" "${E2E_SSH_USER}@${SSH_HOST}" "command -v fio" 2>/dev/null; then
    fio_ready=true
    break
  fi
  sleep 5
done
if [ "$fio_ready" != true ]; then
  echo "provision.sh: ${VM_NAME} (${SSH_HOST}) fio not installed after cloud-init reported done" >&2
  exit 1
fi

echo "vm_name=${VM_NAME}"
# Every *device_name is emitted exactly as status.storage.disks[].deviceName reports it.
echo "device_name=mws-${DEVICE_NAME}"
echo "ssh_host=${SSH_HOST}"
if [ "$E2E_TIER" = smoke ]; then
  echo "independent_device_name=mws-${INDEPENDENT_DEVICE_NAME}"
else
  echo "noname_device_name=${NONAME_DEVICE_NAME}"
  echo "churn_device_name=mws-${CHURN_DEVICE_NAME}"
  echo "storage_disk_boot=${BOOT_ENTRY}"
  echo "storage_disk_data=${DATA_ENTRY}"
  echo "storage_disk_noname=${NONAME_ENTRY}"
  echo "storage_disk_churn=${CHURN_ENTRY}"
fi
