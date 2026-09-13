#!/usr/bin/env bash
# Provisions one ephemeral VM and disk for the full E2E tier.
#
# All environment-specific identifiers are supplied at runtime.
set -euo pipefail

: "${MWS_PROJECT:?}" "${MWS_ZONE:?}" "${E2E_RUN_ID:?}" "${E2E_SUBNET_ID:?}" \
	"${E2E_BOOT_IMAGE:?}" "${E2E_VM_TYPE:?}" "${E2E_AGENT_SA:?}" "${E2E_SSH_PUBLIC_KEY:?}" \
	"${E2E_SSH_USER:?}" "${E2E_SSH_KEY_PATH:?}"

VM_NAME="e2e-full-${E2E_RUN_ID}"
BOOT_DISK="${VM_NAME}-boot"
DATA_DISK="${VM_NAME}-data"
DEVICE_NAME="e2edata"
# NONAME_DISK carries no deviceName in --storage-disks, so the platform
# defaults the guest device to mws-{name} instead of mws-{deviceName}
# (docs/platform-reference.md's "Guest device mapping") — covers that
# default live, alongside NONAME_ALIAS's explicit deviceName above.
NONAME_DISK="${VM_NAME}-noname"
NONAME_ALIAS="e2enoname"
# CHURN_DISK is created but deliberately left unattached — discovery/scope
# scenarios attach and detach it live via AttachChurnDisk/DetachChurnDisk
# (e2e/churn.go). Its deviceName is always explicit: leaving it unset would
# default to mws-{alias}, the exact default NONAME_DISK above already
# exercises, and two disks on one VM covering that default would make a
# future failure ambiguous about which one triggered it.
CHURN_DISK="${VM_NAME}-churn"
CHURN_DEVICE_NAME="e2echurn"

mws compute disk create "compute/projects/${MWS_PROJECT}/disks/${BOOT_DISK}" \
  --zone "${MWS_ZONE}" \
  --disk-type "compute/diskTypes/nbs-pl2" \
  --size 20GB \
  --iops 1000 \
  --source-image "${E2E_BOOT_IMAGE}" \
  --os-type LINUX

mws compute disk create "compute/projects/${MWS_PROJECT}/disks/${DATA_DISK}" \
  --zone "${MWS_ZONE}" \
  --disk-type "compute/diskTypes/nbs-pl2" \
  --size 20GB \
  --iops 1000

mws compute disk create "compute/projects/${MWS_PROJECT}/disks/${NONAME_DISK}" \
  --zone "${MWS_ZONE}" \
  --disk-type "compute/diskTypes/nbs-pl2" \
  --size 20GB \
  --iops 1000

mws compute disk create "compute/projects/${MWS_PROJECT}/disks/${CHURN_DISK}" \
  --zone "${MWS_ZONE}" \
  --disk-type "compute/diskTypes/nbs-pl2" \
  --size 20GB \
  --iops 1000

# Raw-device fio requires passwordless sudo for the test user.
USER_DATA=$'#cloud-config\nusers:\n  - name: '"${E2E_SSH_USER}"$'\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    shell: /bin/bash\n    ssh-authorized-keys:\n      - '"${E2E_SSH_PUBLIC_KEY}"$'\npackage_update: true\npackages:\n  - fio\n  - sysstat\n  - jq'

# Captured (rather than inlined) so they can be echoed verbatim below: the
# Go side resends the VM's exact known-good --storage-disks list whenever it
# attaches or detaches the churn disk (mws compute vm update --storage-disks
# resends the whole list — docs/ci-cd.md), and must not reconstruct these
# strings independently of the ones provision.sh actually used to create it.
BOOT_ENTRY="{name: boot, boot: true, deviceName: bootdisk, disk: {ref: \"compute/projects/${MWS_PROJECT}/disks/${BOOT_DISK}\"}}"
DATA_ENTRY="{name: data, boot: false, deviceName: ${DEVICE_NAME}, disk: {ref: \"compute/projects/${MWS_PROJECT}/disks/${DATA_DISK}\"}}"
NONAME_ENTRY="{name: ${NONAME_ALIAS}, boot: false, disk: {ref: \"compute/projects/${MWS_PROJECT}/disks/${NONAME_DISK}\"}}"
# Rendered now so the Go side never needs its own "-churn" naming
# convention; CHURN_DISK is not passed to --storage-disks here since it
# starts out unattached.
CHURN_ENTRY="{name: churn, boot: false, deviceName: ${CHURN_DEVICE_NAME}, disk: {ref: \"compute/projects/${MWS_PROJECT}/disks/${CHURN_DISK}\"}}"

mws compute virtual-machine create "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" \
  --zone "${MWS_ZONE}" \
  --vm-type "${E2E_VM_TYPE}" \
  --service-account "${E2E_AGENT_SA}" \
  --os-metadata-attributes "user-data: |
$(sed 's/^/  /' <<< "$USER_DATA")" \
  --network-interfaces "name: eth0, primary: true, addresses: [{address: {spec: {subnet: \"${E2E_SUBNET_ID}\"}}, oneToOneNat: {external: {address: {spec: {}}}}}]" \
  --storage-disks "$BOOT_ENTRY" \
  --storage-disks "$DATA_ENTRY" \
  --storage-disks "$NONAME_ENTRY"

EXTERNAL_IP=""
NONAME_DEVICE_NAME=""
# status.storage.disks[].deviceName is a required response field once the
# attachment exists (docs/platform-reference.md), but re-fetch a few times
# rather than assume it (and the external IP) are populated the instant
# creation returns.
for _ in $(seq 1 6); do
  VM_JSON=$(mws compute virtual-machine get "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" -f json)
  EXTERNAL_IP=$(jq -r '.status.network.networkInterfaces[0].addresses[0].oneToOneNat.external.ipAddress' <<< "$VM_JSON")
  NONAME_DEVICE_NAME=$(jq -r --arg alias "$NONAME_ALIAS" \
    '.status.storage.disks[] | select(.name == $alias) | .deviceName' <<< "$VM_JSON")
  if [ -n "$EXTERNAL_IP" ] && [ "$EXTERNAL_IP" != "null" ] \
    && [ -n "$NONAME_DEVICE_NAME" ] && [ "$NONAME_DEVICE_NAME" != "null" ]; then
    break
  fi
  sleep 5
done

if [ -z "$EXTERNAL_IP" ] || [ "$EXTERNAL_IP" = "null" ]; then
  echo "provision.sh: could not resolve external IP for ${VM_NAME}" >&2
  exit 1
fi
if [ -z "$NONAME_DEVICE_NAME" ] || [ "$NONAME_DEVICE_NAME" = "null" ]; then
  echo "provision.sh: could not resolve the server-assigned deviceName for ${VM_NAME}'s ${NONAME_ALIAS} disk" >&2
  exit 1
fi
# Target.DeviceName is the exact status.storage.disks[].deviceName value
# already queried above (docs/design.md §4.1), kept as-is rather than
# transformed.

SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes -i "${E2E_SSH_KEY_PATH:?}")

# Wait for SSH before checking guest readiness.
ssh_up=false
for _ in $(seq 1 30); do
  if ssh "${SSH_OPTS[@]}" "${E2E_SSH_USER}@${EXTERNAL_IP}" true 2>/dev/null; then
    ssh_up=true
    break
  fi
  sleep 10
done
if [ "$ssh_up" != true ]; then
  echo "provision.sh: ${VM_NAME} (${EXTERNAL_IP}) never became SSH-reachable" >&2
  exit 1
fi

# SSH can precede cloud-init completion; verify both status and fio directly.
cloud_init_out=""
for _ in $(seq 1 18); do
  cloud_init_out=$(ssh "${SSH_OPTS[@]}" "${E2E_SSH_USER}@${EXTERNAL_IP}" "cloud-init status" 2>/dev/null || true)
  case "$cloud_init_out" in
    *"status: done"*|*"status: error"*) break ;;
  esac
  sleep 10
done
if [[ "$cloud_init_out" != *"status: done"* ]]; then
  echo "provision.sh: ${VM_NAME} (${EXTERNAL_IP}) cloud-init did not report done: ${cloud_init_out}" >&2
  exit 1
fi

fio_ready=false
for _ in $(seq 1 6); do
  if ssh "${SSH_OPTS[@]}" "${E2E_SSH_USER}@${EXTERNAL_IP}" "command -v fio" 2>/dev/null; then
    fio_ready=true
    break
  fi
  sleep 5
done
if [ "$fio_ready" != true ]; then
  echo "provision.sh: ${VM_NAME} (${EXTERNAL_IP}) fio not installed after cloud-init reported done" >&2
  exit 1
fi

echo "vm_name=${VM_NAME}"
# device_name and churn_device_name are emitted as VM status reports them,
# to match noname_device_name, which already carries that exact value as
# queried from the live VM status above.
echo "device_name=mws-${DEVICE_NAME}"
echo "noname_device_name=${NONAME_DEVICE_NAME}"
echo "churn_device_name=mws-${CHURN_DEVICE_NAME}"
echo "ssh_host=${EXTERNAL_IP}"
echo "storage_disk_boot=${BOOT_ENTRY}"
echo "storage_disk_data=${DATA_ENTRY}"
echo "storage_disk_noname=${NONAME_ENTRY}"
echo "storage_disk_churn=${CHURN_ENTRY}"
