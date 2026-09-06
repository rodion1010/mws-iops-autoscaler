# Development and test environment

This document defines credential handling and the topology required for manual and E2E validation.
It deliberately does not identify a live project, resource names, network ranges, public addresses or
access users. Operational inventory changes independently of the repository and belongs in the
authorized CI/CD secret and environment configuration.

## Credential handling

Use the standard MWS SDK credential chain:

- `MWS_PROJECT` selects the dedicated test project.
- `MWS_ZONE` selects its zone.
- `MWS_SERVICE_ACCOUNT_AUTHORIZED_KEY_PATH` points to a service-account authorized key stored
  outside the repository.
- `MWS_TIMEOUT` should be long enough for infrastructure operations; 60 seconds is the tested local
  value.

Do not copy credential material into `.env`, source code, documentation, a coding-agent prompt or a
chat transcript. Local ignored files are available only to the local session. Cloud coding sessions
must receive credentials through the session's secret store or environment injection.

Grant the credential access only to the dedicated test project. If an experiment requires project
administrator permissions, use a separate test-only service account and do not reuse it elsewhere.

Never commit:

- service-account key JSON;
- IAM or monitoring API tokens;
- SSH private keys;
- identifiers and access details of the live test environment;
- public addresses or ready-to-run access commands;
- generated CLI credential state.

## Required test topology

The reference environment must provide:

- a dedicated, disposable MWS project;
- a network and subnet with outbound internet access;
- an explicit ingress rule when manual SSH access is required;
- an Ubuntu 24.04 VM with at least 2 vCPU;
- an NBS-PL2 boot disk and an independently managed NBS-PL2 data disk, both starting at 1000 IOPS;
- a service account authorized for the exact API operations exercised by the test.

A 2-vCPU VM has a 6000-IOPS aggregate pool and is useful for exercising the VM ceiling with two
disks. E2E cleanup must be scoped to resources created by that run; it must never enumerate a broad
project and delete resources by name alone.

## Runtime discovery and identity

Before every manual or E2E run:

1. Read the current project and zone from the credential environment.
2. Discover the VM and attached disks through the API.
3. Record each disk's current `metadata.id`; a same-name resource may be a different incarnation.
4. Resolve the guest device through `/dev/disk/by-id/mws-<deviceName>` and verify that its kernel
   device exists in `/proc/diskstats`.
5. Re-read live state before cleanup and refuse to delete a resource whose incarnation changed.

## Verified environment behaviour

- A new network denies inbound traffic by default; explicit ingress is required for SSH.
- `admin` and `root` are rejected as cloud-init usernames.
- The `/dev/disk/by-id/mws-<deviceName>` symlink resolves to the attached disk. An additional
  observed alias is `/dev/disk/by-id/scsi-0MWS_PersistentDisk_mws-<deviceName>`.
- The instance metadata token endpoint works from inside the guest.

Detailed platform behaviour and API contracts are recorded in
[platform-reference.md](platform-reference.md). How the automated test tiers built on this reference
environment are organized and what each one covers is documented in [testing.md](testing.md); the
CI/CD pipeline that runs them is in [ci-cd.md](ci-cd.md).
