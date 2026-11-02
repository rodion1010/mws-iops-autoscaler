# MWS IOPS Autoscaler

**English** | [Русский](README.ru.md)

[![CI](https://github.com/rodion1010/mws-iops-autoscaler/actions/workflows/ci.yml/badge.svg)](https://github.com/rodion1010/mws-iops-autoscaler/actions/workflows/ci.yml)
![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

MWS IOPS Autoscaler is an external agent for **MWS Cloud Compute** that watches the real demand of
attached NBS-PL2 disks and adjusts their provisioned IOPS to match it — raising the limit when a disk
is saturated, lowering it after sustained quiet so you stop paying for unused headroom. It is intended
for workloads whose storage demand changes over time: instead of provisioning the peak IOPS limit from
the start, you define a safe range and let the agent track the workload's actual needs within it.

**Contents:** [What it does](#what-it-does) · [Prerequisites](#prerequisites) · [Quick start](#quick-start) · [Configuration](#configuration-essentials) · [CLI options](#command-line-options) · [Observability](#observability) · [Troubleshooting](#troubleshooting) · [Development](#development) · [License](#license)

## What it does

One agent runs inside each target MWS VM. On every control-loop cycle, it:

1. identifies the MWS VM on which it is running;
2. discovers its attached NBS-PL2 disks and determines whether each is
   [dependent or independent](https://mws.ru/docs/cloud-platform/compute/general/disks-spec-ref-overview.html);
3. reads per-disk I/O counters from Linux `/proc/diskstats`;
4. waits for a complete observation window and detects saturation or sustained underutilization from
   utilization, queue and latency signals;
5. estimates the required IOPS and applies the increase or decrease through the appropriate MWS Compute API;
6. records the decision and exposes health and Prometheus telemetry.

### Available now

| Capability | Current behavior |
|---|---|
| Cloud and disk type | MWS Cloud Compute, attached NBS-PL2 disks |
| Deployment model | One local agent per target Linux VM |
| Scaling direction | Bidirectional: raises the limit on saturation, lowers it after sustained quiet |
| Metrics | Guest metrics read from `/proc/diskstats` |
| Disk selection | Automatic discovery, optional exclusions, or an explicit selected list |
| Safety | Per-disk bounds, 100-IOPS rounding, cooldown, trailing 24-hour change budget, concurrent-update protection and dry-run |
| VM capacity | Respects the MWS VM pool of `3000 × vCPU` and accounts for sibling-disk demand |
| Operations | JSON Lines decision journal, Prometheus metrics, liveness and readiness endpoints |

## Prerequisites

- Enable the Compute service in your MWS Cloud project.
- Create a Linux MWS VM and attach at least one **NBS-PL2** disk. Make sure the guest exposes each
  managed disk as `/dev/disk/by-id/mws-<device_name>` and exposes `/proc/diskstats`.
- Create a service account, grant it `compute.editor` in the project and attach it to the VM. If the
  agent will manage dependent disks, also grant the service account `iam.serviceAccount.admin` on
  itself.
- Make sure the VM can reach the instance metadata service at `169.254.169.254` and the MWS API at
  `https://api.mwsapis.ru`.
- To build from source, install Go **1.26 or newer** and Git.
- Prepare a writable persistent directory for the 24-hour change budget. The default is
  `/var/lib/mws-iops-autoscaler`.

## Quick start

Start with an interactive dry run on the target VM. It exercises discovery, sampling and decisions
without changing provisioned IOPS.

### 1. Prepare the MWS resources

1. In the MWS console, enable Compute for the project and create a service account.
2. In the project's IAM section, grant the service account `compute.editor`.
3. If the agent will manage dependent disks, open **IAM → Service accounts**, select the agent's
   service account and, on the **Доступы к ресурсу** (resource access) tab, grant that same service account
   `iam.serviceAccount.admin`.
4. [Attach the service account to the VM](https://mws.ru/docs/cloud-platform/compute/general/vm-link-sa.html)
   in the console or with the MWS CLI:

   ```bash
   mws compute vm update <vm-name> --service-account <service-account>
   ```

### 2. Verify the VM prerequisites

Run these commands inside the target VM:

```bash
# Verify that the attached service account can obtain a metadata token.
curl -fsS -o /dev/null \
  -H 'Metadata-Flavor: Google' \
  http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token

# Verify that the kernel exports disk counters.
test -r /proc/diskstats
```

### 3. Build the binary

```bash
git clone https://github.com/rodion1010/mws-iops-autoscaler.git
cd mws-iops-autoscaler
mkdir -p bin
go build -o bin/mws-iops-autoscaler ./cmd/mws-iops-autoscaler
```

You can also build elsewhere for the target Linux architecture and copy the resulting binary to
the VM.

### 4. Create a configuration

Save the following as `config.yaml`. The example excludes the boot disk with `device_name`
`mws-bootdisk`; replace it with the boot disk's `device_name` from your VM status. The agent
manages every other supported disk attached to the VM:

```yaml
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local

policy:
  limits:
    min_iops: 1000
    max_iops: 10000

disks:
  mode: auto-discovery
  exclude:
    - mws-bootdisk

runtime:
  metrics:
    source: guest
    collection_interval: 10s
  controller:
    reconcile_interval: 30s
    discovery_interval: 5m
```

To manage only named disks, use explicit selection instead:

```yaml
disks:
  mode: selected
  selected:
    - device_name: mws-datadisk
```

See the [configuration reference](docs/configuration.md) for validation rules, per-disk limits and the
experimental `advanced` settings. Start with the defaults unless you have dry-run data that justifies
tuning them.

### 5. Validate the configuration

```bash
./bin/mws-iops-autoscaler \
  -config ./config.yaml \
  -validate-config
```

A valid configuration exits with status 0 and prints a one-line summary. Invalid input exits with
status 1 and reports every detected violation.

### 6. Start in dry-run mode

Create a local state directory and start the agent:

```bash
mkdir -p ./var

./bin/mws-iops-autoscaler \
  -config ./config.yaml \
  -dry-run \
  -state-dir ./var \
  -journal ./decisions.jsonl \
  -listen 127.0.0.1:9435
```

Leave it running for at least one complete observation window. With the defaults, a scale-up
decision needs 5 minutes of data and a scale-down decision needs 30 minutes. Generate a representative
workload and inspect the journal for proposed actions and the reasons for no action:

```bash
tail -f ./decisions.jsonl
```

Dry-run never mutates disk IOPS. For independent disks it can ask the API to validate the proposed
write; for dependent disks the verdict is computed locally.

### 7. Check health and metrics

In another shell:

```bash
curl -fsS http://127.0.0.1:9435/healthz
curl -fsS http://127.0.0.1:9435/readyz
curl -fsS http://127.0.0.1:9435/metrics | head
```

`/healthz` returns HTTP 200 while the process is alive. `/readyz` returns HTTP 200 only when all of
the following hold:

- the configuration is valid;
- discovery has succeeded and the managed set is non-empty;
- the latest metrics collection call has succeeded;
- the durable change budget is healthy.

Otherwise `/readyz` returns HTTP 503 with a JSON breakdown. A successful collection call can still
yield no sample for an individual disk: use the decision journal to diagnose `no_data` and staleness
reasons.

### 8. Enable writes

After the dry-run decisions match your expectations, stop the process and start it again without
`-dry-run`:

```bash
./bin/mws-iops-autoscaler \
  -config ./config.yaml \
  -state-dir /var/lib/mws-iops-autoscaler \
  -journal /var/log/mws-iops-autoscaler-decisions.jsonl \
  -listen 127.0.0.1:9435
```

Ensure the process user can create and write both paths. Keep the state directory persistent across
restarts: `budget.jsonl` is the durable record that enforces the trailing 24-hour change limit.

> [!WARNING]
> IOPS changes persist in MWS after the agent stops. There is no dead-man switch that restores the
> previous limit or triggers scale-down; restart the agent promptly or adjust the disk manually.

Run the binary under your own process supervisor:

- run exactly one agent for a target VM;
- restart on failure and stop with `SIGTERM`;
- preserve the state directory;
- collect the decision journal and scrape `/metrics`;
- do not expose the HTTP listener publicly without network-level protection.

## Configuration essentials

- `policy.limits.min_iops` and `max_iops` define the allowed range. MWS NBS-PL2 accepts 1000–10000
  IOPS in steps of 100.
- `disks.mode: auto-discovery` manages discovered supported disks except entries under `exclude`.
- `disks.mode: selected` manages only entries under `selected`.
- Disk names are the platform-visible MWS `device_name` values (the `mws-` prefix included), not
  Linux `/dev` paths.
- The agent detects the disk attachment class itself; you do not configure the write path.
- `runtime.metrics.source` is `guest`.
- The agent changes nothing when metrics or discovery are stale, the observation window is
  incomplete, the disk is neither saturated nor underutilized for the whole window, a cooldown or the
  change budget applies, the disk identity cannot be verified, or sibling disks leave too little VM
  headroom.

## Command-line options

| Option | Description | Default |
|---|---|---|
| `-config <path>` | Configuration file; required | — |
| `-validate-config` | Validate the configuration and exit | off |
| `-dry-run` | Discover, sample and decide without applying a change | off |
| `-state-dir <path>` | Persistent directory for the 24-hour change budget | `/var/lib/mws-iops-autoscaler` |
| `-journal <path>` | JSON Lines decision journal; stdout when omitted | stdout |
| `-listen <addr>` | Address serving `/metrics`, `/healthz` and `/readyz` | `:9435` |

Local mode resolves its own project, zone and VM name from the VM's instance metadata service before
starting. The MWS Go SDK separately obtains credentials from the service account attached to the VM,
also through the instance metadata service.

## Observability

The decision journal emits one JSON object per managed disk per reconciliation cycle, including the
current and proposed IOPS, action, reason, utilization, demand, latency, VM headroom, window fill and
remaining change budget. Check it first when you need to know why the agent changed a disk or left it
unchanged.

Prometheus metrics include:

- `iops_autoscaler_disk_provisioned_iops` and `iops_autoscaler_disk_proposed_target_iops`;
- `iops_autoscaler_disk_observed_iops`, utilization, saturation and demand;
- `iops_autoscaler_vm_headroom_iops` and `iops_autoscaler_vm_cap_reached`;
- sampling freshness, window fill and remaining change budget;
- skipped or configured-but-unmanaged disks and durable-budget health.

See [design: observability](docs/design.md#7-observability) for the complete telemetry contract and
the signals considered alert-worthy. Ready-to-use alert rules are not shipped; configure monitoring
for the relevant signals. Ownership and escalation policy are tracked in
[issue #59](https://github.com/rodion1010/mws-iops-autoscaler/issues/59).

## Troubleshooting

| Symptom | What to check |
|---|---|
| Cannot resolve VM identity or credentials | Run inside the target MWS VM; verify its attached service account and metadata access as in [step 2](#2-verify-the-vm-prerequisites). |
| Compute API returns 403 | Verify that Compute is enabled and the service account has `compute.editor`. For a dependent disk, also check the `iam.serviceAccount.admin` grant from [step 1](#1-prepare-the-mws-resources). |
| `/readyz` reports `discovery_succeeded: false` | Check API connectivity, permissions and that at least one supported attached disk matches the selection. |
| `/readyz` reports `source_reachable: false` | Check `/proc/diskstats` and the `/dev/disk/by-id/mws-<device_name>` symlinks. |
| `configured_disks_unmanaged` appears in `/readyz` | A selected or excluded name no longer resolves. Fix the name or disk attachment. This is reported as degradation while the agent stays ready, so alert on it: [alerting contract](docs/design.md#73-health-and-alerting). |
| No change occurs | Wait for the observation window, then inspect the decision `reason`: the disk may be neither saturated nor underutilized, or a safety guardrail may be active. |
| `budget_healthy: false` | The durable state could not be trusted. Fix storage permissions or filesystem errors and restart the agent. |
| Disk is capped while demand remains high | Check the per-disk maximum and `iops_autoscaler_vm_cap_reached`; add vCPU if the VM-wide IOPS pool is the limit. |

## Development

Run the local test suite with:

```bash
go test ./...
```

Further reading:

- [Documentation index](docs/README.md)
- [Configuration reference](docs/configuration.md)
- [Architecture and safety invariants](docs/design.md)
- [Development environment](docs/development.md)
- [Testing strategy](docs/testing.md)
- [CI/CD](docs/ci-cd.md)

## License

[Apache License 2.0](LICENSE)
