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

# Raw-device fio requires passwordless sudo for the test user.
USER_DATA=$'#cloud-config\nusers:\n  - name: '"${E2E_SSH_USER}"$'\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    shell: /bin/bash\n    ssh-authorized-keys:\n      - '"${E2E_SSH_PUBLIC_KEY}"$'\npackage_update: true\npackages:\n  - fio\n  - sysstat\n  - jq'

mws compute virtual-machine create "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" \
  --zone "${MWS_ZONE}" \
  --vm-type "${E2E_VM_TYPE}" \
  --service-account "${E2E_AGENT_SA}" \
  --os-metadata-attributes "user-data: |
$(sed 's/^/  /' <<< "$USER_DATA")" \
  --network-interfaces "name: eth0, primary: true, addresses: [{address: {spec: {subnet: \"${E2E_SUBNET_ID}\"}}, oneToOneNat: {external: {address: {spec: {}}}}}]" \
  --storage-disks "{name: boot, boot: true, deviceName: bootdisk, disk: {ref: \"compute/projects/${MWS_PROJECT}/disks/${BOOT_DISK}\"}}" \
  --storage-disks "{name: data, boot: false, deviceName: ${DEVICE_NAME}, disk: {ref: \"compute/projects/${MWS_PROJECT}/disks/${DATA_DISK}\"}}"

EXTERNAL_IP=$(mws compute virtual-machine get "compute/projects/${MWS_PROJECT}/virtualMachines/${VM_NAME}" -f json \
  | jq -r '.status.network.networkInterfaces[0].addresses[0].oneToOneNat.external.ipAddress')

if [ -z "$EXTERNAL_IP" ] || [ "$EXTERNAL_IP" = "null" ]; then
  echo "provision.sh: could not resolve external IP for ${VM_NAME}" >&2
  exit 1
fi

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
echo "device_name=${DEVICE_NAME}"
echo "ssh_host=${EXTERNAL_IP}"
