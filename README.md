# MWS IOPS Autoscaler

MWS IOPS Autoscaler is an external agent for **MWS Cloud Compute** that watches the real demand of
attached NBS-PL2 disks and raises their provisioned IOPS when they are saturated. It is intended for
workloads whose storage demand changes over time: instead of provisioning the peak IOPS limit from
the start, you define a safe range and let the agent increase the limit when the workload needs it.
The current scale-up-only release focuses on reducing throttling risk; it does not continuously
optimize storage cost.

> [!IMPORTANT]
> The current release is **local mode, scale-up only**. Run one agent inside each target MWS virtual
> machine. Once the agent raises a disk's IOPS, the new limit remains in effect when the workload
> subsides and even if the agent stops. Lower it manually when appropriate. Automatic scale-down,
> fleet mode and platform metrics are not implemented yet.

## What it does

On every control-loop cycle, the agent:

1. identifies the MWS VM on which it is running;
2. discovers its attached NBS-PL2 disks and classifies them as dependent or independent;
3. reads per-disk activity from Linux `/proc/diskstats`;
4. waits for a complete observation window and detects actual saturation from utilization, queue and
   latency signals;
5. estimates the required IOPS and applies the increase through the appropriate MWS Compute API;
6. records the decision and exposes health and Prometheus telemetry.

Both MWS attachment models are supported. An **independent** disk is updated through the disk API; a
**dependent** disk is part of the VM specification and is updated through the VM API.

### Available now

| Capability | Current behavior |
|---|---|
| Cloud and disk type | MWS Cloud Compute, attached NBS-PL2 disks |
| Deployment model | One local agent per target Linux VM |
| Scaling direction | Scale-up only |
| Metrics | Guest metrics from `/proc/diskstats`; no Monitoring API key required |
| Disk selection | Automatic discovery, optional exclusions, or an explicit selected list |
| Safety | Per-disk bounds, 100-IOPS rounding, cooldown, trailing 24-hour change budget, concurrent-update protection and dry-run |
| VM capacity | Respects the MWS VM pool of `3000 × vCPU` and accounts for sibling-disk demand |
| Operations | JSON Lines decision journal, Prometheus metrics, liveness and readiness endpoints |

## Prerequisites

Before starting the agent, prepare the following:

- An **MWS Cloud project** with the Compute service enabled, and the availability zone containing the
  target VM. Set both as `MWS_PROJECT` and `MWS_ZONE`; without `MWS_ZONE`, the MWS SDK defaults to
  `ru-central1-a`.
- A Linux MWS VM with at least one attached **NBS-PL2** disk. The guest must expose each managed disk
  as `/dev/disk/by-id/mws-<device_name>` and expose `/proc/diskstats`.
- An MWS service account attached to that VM. Grant it `compute.editor` in the project so the agent can
  discover the VM and disks and update their IOPS.
- Access from the VM to the instance metadata service at `169.254.169.254` and to the MWS API at
  `https://api.mwsapis.ru`.
- Go **1.26 or newer** and Git if you build the binary from source. They are not runtime dependencies
  after the binary has been built.
- A writable persistent directory for the 24-hour change budget. The default is
  `/var/lib/mws-iops-autoscaler`.

> [!WARNING]
> Updating a dependent disk currently makes MWS re-check access to the service account attached to the
> VM. Live testing found that `compute.editor` alone is insufficient: the calling service account also
> needs `iam.serviceAccount.admin` **on that attached service-account resource**, even though the update
> does not change the VM identity. This is an upstream permission issue and grants broader access than
> expected. Prefer an independent disk if that workaround is unacceptable.

## Quick start

The safest first launch is an interactive dry run on the target VM. It exercises discovery, sampling
and decisions without changing provisioned IOPS.

### 1. Prepare the MWS resources

In the MWS console, enable Compute for the project, create a service account, grant it
`compute.editor` in that project and attach it to the target VM. The equivalent VM update with the
MWS CLI is:

```bash
mws compute vm update <vm-name> --service-account <service-account>
```

Role assignments can be made in the project's IAM section. Follow the dependent-disk warning under
Prerequisites only if the agent must manage that attachment class. The MWS documentation also covers
[linking a service account to a VM](https://mws.ru/docs/cloud-platform/compute/general/vm-link-sa.html).

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

Save the following as `config.yaml`. This example assumes the boot disk is named `bootdisk`;
change that value only if you assigned it a different device name. Every other supported disk attached
to the VM is managed automatically:

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
    - bootdisk

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
    - device_name: datadisk
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

Set the project name used by the MWS SDK, create a local state directory and start the agent:

```bash
export MWS_PROJECT='<project-name>'
export MWS_ZONE='<vm-zone>'
mkdir -p ./var

./bin/mws-iops-autoscaler \
  -config ./config.yaml \
  -dry-run \
  -state-dir ./var \
  -journal ./decisions.jsonl \
  -listen 127.0.0.1:9435
```

Leave it running for at least one complete scale-up window—five minutes with the defaults. Generate a
representative workload and inspect the journal to see both proposed actions and reasons for no action:

```bash
tail -f ./decisions.jsonl
```

Dry-run never mutates disk IOPS. For independent disks it can ask the API to validate the proposed
write; for dependent disks the verdict is computed locally because the platform has no equivalent
validation-only VM update.

### 7. Check health and metrics

In another shell:

```bash
curl -fsS http://127.0.0.1:9435/healthz
curl -fsS http://127.0.0.1:9435/readyz
curl -fsS http://127.0.0.1:9435/metrics | head
```

`/healthz` returns HTTP 200 while the process is alive. `/readyz` returns HTTP 200 only after the
configuration is valid, discovery has succeeded, the managed set is non-empty, the latest metrics
collection call has succeeded and the durable change budget is healthy; otherwise it returns HTTP
503 with a JSON breakdown. A successful collection call can still yield no sample for an individual
disk, so use the decision journal to diagnose `no_data` and staleness reasons.

### 8. Enable writes

After the dry-run decisions match your expectations, stop the process and start it again without
`-dry-run`:

```bash
export MWS_PROJECT='<project-name>'
export MWS_ZONE='<vm-zone>'

./bin/mws-iops-autoscaler \
  -config ./config.yaml \
  -state-dir /var/lib/mws-iops-autoscaler \
  -journal /var/log/mws-iops-autoscaler-decisions.jsonl \
  -listen 127.0.0.1:9435
```

Ensure the process user can create and write both paths. Keep the state directory persistent across
restarts: `budget.jsonl` is the durable record that enforces the trailing 24-hour change limit.

The repository does not currently ship a package or service unit. When placing the binary under your
own process supervisor:

- pass `MWS_PROJECT` and `MWS_ZONE` explicitly;
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
- Disk names are MWS `device_name` values without the `mws-` prefix, not Linux `/dev` paths.
- Disk attachment class is detected automatically; users do not configure the write path.
- `runtime.metrics.source` must currently be `guest`. `platform` is rejected because that source is not
  implemented yet.
- Scale-up is blocked when metrics or discovery are stale, the observation window is incomplete, the
  disk is not saturated, cooldown or the change budget applies, identity cannot be verified, or sibling
  demand leaves insufficient VM headroom.

## Command-line options

| Option | Description | Default |
|---|---|---|
| `-config <path>` | Configuration file; required | — |
| `-validate-config` | Validate the configuration and exit | off |
| `-dry-run` | Discover, sample and decide without applying a change | off |
| `-state-dir <path>` | Persistent directory for the 24-hour change budget | `/var/lib/mws-iops-autoscaler` |
| `-journal <path>` | JSON Lines decision journal; stdout when omitted | stdout |
| `-listen <addr>` | Address serving `/metrics`, `/healthz` and `/readyz` | `:9435` |

`MWS_PROJECT` and `MWS_ZONE` select the project and availability zone of the target VM. Set both
explicitly at runtime; if `MWS_ZONE` is omitted, the SDK defaults to `ru-central1-a`. They are
intentionally environment variables rather than command-line flags. The MWS Go SDK obtains
credentials from the service account attached to the VM through the instance metadata service.

## Observability

The decision journal emits one JSON object per managed disk per reconciliation cycle, including the
current and proposed IOPS, action, reason, utilization, demand, latency, VM headroom, window fill and
remaining change budget. It is the first place to look when asking “why did the agent scale—or why did
it not scale?”

Prometheus metrics include:

- `iops_autoscaler_disk_provisioned_iops` and `iops_autoscaler_disk_proposed_target_iops`;
- `iops_autoscaler_disk_observed_iops`, utilization, saturation and demand;
- `iops_autoscaler_vm_headroom_iops` and `iops_autoscaler_vm_cap_reached`;
- sampling freshness, window fill and remaining change budget;
- skipped or configured-but-unmanaged disks and durable-budget health.

See [design: observability](docs/design.md#7-observability) for the complete telemetry contract and
the signals considered alert-worthy. Ready-to-use alert rules and their ownership and escalation
policy are not defined yet; this is tracked in [issue #59](https://github.com/rodion1010/mws-iops-autoscaler/issues/59).

## Troubleshooting

| Symptom | What to check |
|---|---|
| `MWS_PROJECT must be set` | Export the exact MWS project name in the agent process environment. |
| VM cannot be found during initial discovery | Verify that `MWS_ZONE` exactly matches the target VM's availability zone. |
| Cannot resolve VM identity or credentials | Run inside the target MWS VM; verify its attached service account and metadata access with the command above. |
| Compute API returns 403 | Verify that Compute is enabled and the service account has `compute.editor`. For a dependent disk, also check the upstream IAM workaround in Prerequisites. |
| `/readyz` reports `discovery_succeeded: false` | Check API connectivity, permissions and that at least one supported attached disk matches the selection. |
| `/readyz` reports `source_reachable: false` | Check `/proc/diskstats` and the `/dev/disk/by-id/mws-<device_name>` symlinks. |
| `configured_disks_unmanaged` appears in `/readyz` | A selected or excluded name no longer resolves. Fix the name or disk attachment. This is reported as degradation without making the whole agent unready. |
| No scale-up occurs | Wait for the observation window, then inspect the decision `reason`; the workload may not be saturated or a safety guardrail may be active. |
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
- [Verified MWS platform behavior](docs/platform-reference.md)
- [Development environment](docs/development.md)
- [Testing strategy](docs/testing.md)
- [CI/CD](docs/ci-cd.md)
