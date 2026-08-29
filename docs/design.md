# MWS IOPS Autoscaler — Design / RFC

**Status:** Draft
**Date:** 2026-08-28
**Author:** generated with Claude Code, reviewed by Product Lead (NBS)

**Derived from `docs/prd-mws-iops-autoscaler.md`, which is the primary document.** The PRD states the
requirements; this document describes how they are met. It does not introduce, widen or drop a
requirement on its own — when a design decision changes one, the PRD is amended in the same change
(see the sync rule in `docs/README.md`).

Companion documents: requirements in `docs/prd-mws-iops-autoscaler.md`, decisions in `docs/adr/`,
measured platform behaviour in `CLAUDE.md`, configuration model in `docs/configuration.md`.

---

## 1. Summary

MWS bills NBS-PL2 IOPS as **provisioned, not consumed**. A user picks a limit between 1000 and 10000
and pays for it whether or not the workload uses it. Too low throttles; too high wastes money. Today
the only remedy is a human watching graphs.

The autoscaler closes that loop. It finds the **minimum provisioned IOPS at which the workload shows
no signs of storage saturation**:

```
minimize(provisioned_iops)
subject to:  no sustained queueing
             latency within baseline
             no throttling
             disk limits [1000, 10000], step 100
             VM aggregate limit 3000 × vCPU
```

The critical design commitment: **utilization is not the trigger.** A disk running at 100% of a limit
it already pays for, with flat latency and an empty queue, is efficient. Raising its limit buys
nothing. Only *saturation* — demand that cannot be served — justifies spending more. See ADR-002.

---

## 2. Scope

**In scope (v1)**
- Reactive control loop, two modes (`local`, `fleet`).
- Both disk classes: independent via disk API, dependent via VM spec (ADR-003).
- One built-in default parameter set as the user-facing surface (limits + opt-in `advanced`);
  named presets deferred past the MVP (ADR-006).
- Rollout stages: scale-up-only (with `--dry-run` as the pre-live validation step, not a
  separate release), then bidirectional (ADR-005 amends ADR-002's original staging).

**Out of scope (v1)**
- Forecasting, seasonality, predictive pre-scaling.
- Workload classification.
- Joint IOPS + throughput optimisation.
- Automatic vCPU changes to lift the VM cap — detected and reported, never actioned.
- A UI. Config is a file; output is logs and Prometheus metrics.

---

## 3. Architecture

### 3.1 Components

```
                     ┌──────────────────────────────────────────┐
                     │              Autoscaler                  │
                     │                                          │
  config.yaml ──────▶│  ┌────────────┐                          │
                     │  │   Config   │  defaults, inheritance   │
                     │  │  Resolver  │  → EffectivePolicy/disk  │
                     │  └─────┬──────┘                          │
                     │        │                                 │
                     │  ┌─────▼──────┐   ┌──────────────────┐   │
   Compute API ─────▶│  │ Discovery  │──▶│   Disk Registry  │   │
   (slow path)       │  │            │   │ class, limits,   │   │
                     │  └────────────┘   │ deviceName, VM   │   │
                     │                   └────────┬─────────┘   │
                     │                            │             │
  ┌───────────────┐  │  ┌────────────┐            │             │
  │ MetricSource  │  │  │  Sampler   │◀───────────┘             │
  │  ├ Guest      │─▶│  │            │  ring buffer per disk    │
  │  └ Platform   │  │  └─────┬──────┘                          │
  └───────────────┘  │        │                                 │
                     │  ┌─────▼──────┐                          │
                     │  │  Analyzer  │  windows, percentiles,   │
                     │  │            │  baselines, saturation   │
                     │  └─────┬──────┘                          │
                     │        │                                 │
                     │  ┌─────▼──────┐                          │
                     │  │  Decider   │  demand → target         │
                     │  └─────┬──────┘                          │
                     │        │                                 │
                     │  ┌─────▼──────┐                          │
                     │  │ Guardrails │  clamp, step, ratio,     │
                     │  │            │  cooldown, budget, cap   │
                     │  └─────┬──────┘                          │
                     │        │                                 │
                     │  ┌─────▼──────┐   ┌──────────────────┐   │
  Compute API ◀──────│──│  Actuator  │──▶│  State / Journal │   │
  (write path)       │  │ ├ DiskPath │   └──────────────────┘   │
                     │  │ └ VmPath   │                          │
                     │  └────────────┘                          │
                     │                                          │
                     │  ┌────────────────────────────────────┐  │
                     │  │ Observability: logs, /metrics, /healthz │
                     │  └────────────────────────────────────┘  │
                     └──────────────────────────────────────────┘
```

### 3.2 Component responsibilities

| Component | Responsibility | Key property |
|---|---|---|
| **Config Resolver** | Merge platform default → project → VM → disk into one `EffectivePolicy` per disk | Pure function; fully testable without I/O |
| **Discovery** | Enumerate VMs and disks; classify dependent/independent; resolve `deviceName` → guest path; fetch `vcpuCount` | Slow path, runs on `discovery_interval`, never in the control loop |
| **Disk Registry** | In-memory model of managed disks and their VM grouping | Rebuilt each discovery cycle; no stale entries |
| **MetricSource** | One interface, two implementations (guest, platform) | Explicit units on every field (ADR-001) |
| **Sampler** | Fixed-size ring buffer of samples per disk | Bounded memory; drops oldest |
| **Analyzer** | Rolling windows, percentiles, latency baselines, saturation verdict | Pure over a sample slice |
| **Decider** | Demand estimate → raw target | Pure; no clamping here |
| **Guardrails** | Clamp, round, rate-limit, cooldown, budget, VM headroom | Last line before any write |
| **Actuator** | Apply the change via the correct write path | Only component that mutates anything |
| **State/Journal** | Last action per disk, cooldowns, 24h budget, decision history | In-memory v1; see §9.4 |

### 3.3 Why this shape

The pipeline is deliberately a chain of **pure functions with I/O at the ends**. Discovery and
Actuator touch the network; everything between them is a pure transformation from
`[]Sample + EffectivePolicy + VMState` to a `Decision`. That makes the entire decision logic testable
with table-driven unit tests and no mocks, which matters because the decision logic is where the
product risk lives.

---

## 4. Flows

### 4.1 Discovery (slow path, default 5 min)

```
 for each target VM:
   GET  /compute/v1/projects/{p}/virtualMachines/{vm}
   ├─ read spec.storage.disks[]
   │    disk.spec present → DEPENDENT   (write via VM path)
   │    disk.ref  present → INDEPENDENT (write via disk path)
   ├─ read status.storage.disks[].deviceName → "mws-<name>"
   │    strip the "mws-" prefix → the config identifier (ADR-004)
   │    local mode: resolve /dev/disk/by-id/mws-<name>, verify it exists
   └─ GET /compute/v1/vmTypes/{vmType}
        vm_iops_limit = 3000 × spec.cpu.vcpuCount
        (spec.disks.totalIops is null on every type — do not use)

 for each disk:
   GET /compute/v1/projects/{p}/disks/{d}
   └─ spec.iops (current provisioned), status.iops {base, extra, total}

 apply selection strategy → managed set
   match config selectors against DeviceName (unprefixed) — never a kernel device path,
   which is assigned in discovery order and not stable across reboots (ADR-004)
   fleet: the key is (instance, device_name); deviceName is unique per VM, not globally
 resolve EffectivePolicy per disk
```

Discovery is idempotent and rebuilds the registry wholesale. A disk that changed class, was detached,
or was resized externally is picked up here.

### 4.2 Sampling (fast path, default 10 s)

```
 local:     read /proc/diskstats → delta since previous → ops/s, bytes/s, aqu-sz, await
 fleet:     GET monitoring.mwsapis.ru .../compute:exportPrometheusMetrics
            → parse, index by (vm_name, device_name)
 append Sample{ts, read_ops, write_ops, read_bytes, write_bytes,
                queue_depth+unit, p95_read_lat, p95_write_lat, source} to ring buffer
```

`fleet` sampling is per-project, not per-disk: one HTTP call yields every disk in the project. Polling
faster than the platform's 30 s publication interval yields duplicates — the sampler deduplicates on
`metric_ts` rather than wall clock. The `device_name` label carries the `mws-` prefix, so indexing
uses `DiskRef.MetricLabel()` rather than the config form.

The collection interval is process-wide — one Sampler, one source, one tick — set once by
`runtime.metrics.collection_interval`. There is no per-policy or per-disk override: a single call
already returns every managed disk's sample for that tick, so a disk-level cadence could never
actually be honored (ADR-010).

### 4.3 Reconcile (default 30 s)

```
 for each managed disk:
   if resize_in_progress(disk):        skip
   if in_cooldown(disk):               skip
   if window_not_full(disk):           skip   ← cold start never acts
   if baseline_not_established(disk):  skip   ← latency baseline warm-up

   # window_not_full is timestamp-based, not a sample count (ADR-010):
   #   newest_sample.Timestamp - oldest_sample.Timestamp < policy.evaluation.scale_up_window
   # (scale_down_window for the down-check). A count derived from an assumed
   # collection_interval would silently misjudge readiness under tick jitter
   # or a missed sample — the timestamp span is what "full" actually means.

   m  = analyze(samples, policy.evaluation)
   vm = analyze_vm(sibling disks)

   if saturated(m, policy):
       if vm_headroom_insufficient(vm, disk):
           emit VM_IOPS_CAP_REACHED ; no write
       else:
           target = guardrails(demand(m) / policy.target.scale_up_utilization)
           if target > current: actuate(disk, target); start_cooldown(up)
   elif underutilized(m, policy):
       target = guardrails(p95_iops(m) / policy.target.scale_down_utilization)
       if target < current: actuate(disk, target); start_cooldown(down)
   else:
       no_action(reason)
```

Every branch — including `no_action` — emits a structured decision record with its inputs. "Why
didn't it scale?" must always be answerable from the log alone.

Comparison mode (US-005b) runs this loop twice per disk in `local` mode — once per `MetricSource` —
producing two independent `Decision`s per cycle. Only the `Guest`-sourced branch ever reaches
`actuate()`; the `Platform`-sourced branch always ends in `no_action` regardless of what it computed,
and both are journaled. No new field is needed on `Decision` for this — `Sample.Source` already flows
through into `Analysis`, so the journal attributes each decision by source from data it already has.

### 4.4 Saturation detection

```
resource_gate :=
      iops_utilization      >= policy.saturation.utilization_threshold   (default 0.98)
   OR bandwidth_utilization >= policy.saturation.utilization_threshold

saturated :=
      resource_gate
  AND ( queue_depth      >= policy.saturation.queue_depth_threshold
     OR p95_latency      >  baseline_p95 × policy.saturation.latency.baseline_multiplier
     OR throttling_detected )
  AND sustained over policy.evaluation.scale_up_window
```

`bandwidth_utilization = (read_bytes/s + write_bytes/s) / (spec.iops × 32768)` — NBS-PL2's fixed
IOPS-to-bandwidth ratio (CLAUDE.md: `MB/s = IOPS × 32 / 1024`), using the exact measured bytes/IOPS
figure, not an approximation. `spec.iops` is already read by `Discover` (M1) on the same cycle — no
extra API call.

**Utilization is a gate, not a trigger** (ADR-002) — **on either resource** (ADR-008). If a disk is
not near either of its provisioned limits it cannot be limit-starved, so the expensive checks are
skipped. A large-block, throughput-heavy workload saturates bandwidth long before it saturates IOPS
count; gating on IOPS alone would silently miss it — the whole reason ADR-008 exists.

**Latency is baseline-relative, never absolute.** `p95 > 10 ms` is wrong for everyone; acceptable
latency is workload-specific. The baseline is a long rolling low-percentile of the disk's own
observed latency, frozen while the disk is saturated so a throttled period cannot poison it.

### 4.5 Throttling detection and demand estimation

This is the crux, and it is where `local` mode earns its keep.

**The problem.** At the limit the IOPS counter pins and stops carrying information. A disk reading
1000/1000 might want 1100 or 10000. Scaling by a fixed percentage is guesswork.

**Guest-side (local mode).** `/proc/diskstats` gives what the platform does not: queue depth in
operations, for **both** reads and writes, at 1 s granularity. Throttling has a clean signature:

```
throttled := served_iops ≈ provisioned_limit          (within tolerance)
         AND queue_depth sustained > 0
```

A rate-limited device completes at exactly its cap while requests pile up behind it. That is directly
observable, unlike on the platform side where no throttling counter exists.

**Demand estimate.** Little's Law relates queue length, service rate and wait time. Under throttling
the service rate is pinned, so the excess appears as queueing. Two regimes:

| Regime | Signature | Estimate |
|---|---|---|
| Open workload (arrivals independent) | queue **growing** | `demand ≈ served + dL/dt` |
| Closed workload (bounded concurrency) | queue **stable and deep** | `demand ≈ served × (p95_latency / baseline_p95)` |

The closed-workload form is the workhorse: if latency is 3× baseline, the workload is receiving
roughly a third of the service it wants. Both forms are clamped to
`max_scale_up_ratio` so a pathological reading cannot request a 10× jump.

**Platform-side (fleet mode).** No queue-in-operations and no read queue, so only the
latency-ratio form is available, computed from the exported histograms. Coarser, and one more reason
`local` prefers the guest (ADR-001).

**Honesty note.** These estimators are unvalidated. They are the reason the rollout starts
observe-only: the first stage exists specifically to check whether the estimate matches what actually
happened after a manual resize.

### 4.6 VM headroom

Headroom is **dynamic**, computed from siblings' actual consumption, not their provisioned limits —
sibling disks are bursty, and their limits routinely oversubscribe the pool:

```
vm_headroom       = vm_iops_limit − p95_aggregate_iops_of_other_disks
effective_ceiling = min(platform_max 10000, policy.limits.max_iops, vm_headroom)
```

When a disk is saturated **and** the pool is saturated, the controller must not perform a cosmetic
resize. It emits `VM_IOPS_CAP_REACHED` and reports that the bottleneck is vCPU.

This matters more here than on most platforms: **the VM ceiling is not enforced by the API.** A 2-vCPU
VM with a 6000 pool accepts a disk set to 10000 and bills for it. `validateOnly` does not catch it
either. The guardrail is the only thing preventing billed-but-unusable IOPS.

### 4.7 Actuation

**Independent disk** — single-field partial update, no read-modify-write:

```
POST compute.mwsapis.ru/compute/v1/projects/{p}/disks/{d}?updateOnly=true
Idempotency-Key: <uuid>
{"spec": {"iops": N}}
```

**Dependent disk** — read-modify-write through the VM, which is the riskier path:

```
GET  .../virtualMachines/{vm}                   capture etag, every disk's alias (name)
  build a minimal partial body: every alias, in order, carrying nothing but its name —
  except the target alias, which additionally carries spec.iops
POST .../virtualMachines/{vm}                   send etag, Idempotency-Key
```

Every alias must still be present, because Upsert treats an existing `storage.disks[]` element
missing from the request as a delete — dropping an entry to "only touch the target" would detach
every other disk on the VM (issue #3). But no entry, including the target, carries anything beyond
its collection key (`name`) plus the one field actually changing. Earlier drafts of this path
round-tripped a full disk spec — `boot`, `deviceName`, `diskType`, `size` — reconstructed from the
GET response and resent as an assertion that nothing else had changed. That is strictly worse than
omitting those fields: `name`/`boot`/`deviceName`/`diskType` are immutable on update and the API
accepts them omitted entirely (CLAUDE.md "DiskSpecRequest mutability"), so a value resent through a
lossy response→domain→request translation risks a mismatch the platform would reject or, worse,
silently accept as an unintended change. Sending the collection key alone carries the same
concurrency guarantee (the etag) without manufacturing a value for a field the write never intended
to touch.

`UpdateVirtualMachine` has no `validateOnly` — confirmed against the generated SDK client, which has
no such field on this request (unlike `UpdateDiskRequest`, which does). **This path has no
server-side dry-run** (ADR-007): `--dry-run` can report the computed decision and the agent's own
client-side checks for a dependent disk, but not a platform-confirmed verdict the way it can for an
independent one.

Both paths verify the applied value from the response. A write whose response does not carry the
requested value is a failure, not a success — in-memory state must never claim a change that did not
land.

`?updateOnly=true` is always set: the agent must never create a disk. The SDK's
`DiskSugared.UpdateDisk` already targets that path.

---

## 5. Contracts

### 5.1 External — MWS APIs consumed

| Purpose | Endpoint | Frequency | Credential |
|---|---|---|---|
| VM read | `GET /compute/v1/projects/{p}/virtualMachines/{vm}` | discovery | IAM token |
| VM type | `GET /compute/v1/vmTypes/{t}` | discovery, cached | IAM token |
| Disk read | `GET /compute/v1/projects/{p}/disks/{d}` | discovery | IAM token |
| Disk write | `POST /compute/v1/projects/{p}/disks/{d}?updateOnly=true` | on decision | IAM token |
| VM write | `POST /compute/v1/projects/{p}/virtualMachines/{vm}` | on decision (dependent) | IAM token |
| Metrics | `GET monitoring.../serviceMetrics/compute:exportPrometheusMetrics` | 30 s (`fleet`) | **API key** + `monitoring.metricExporter` |

Three hosts, easily confused: `api.mwsapis.ru` (SDK default), `compute.mwsapis.ru` (disk/VM),
`monitoring.mwsapis.ru` (metrics). Metrics use an **API key**, not the IAM token — an IAM token
returns `503` masking the real `403`.

### 5.2 Internal interfaces

```go
// DiskRef is the identity of a managed disk, resolved once at discovery.
// DeviceName is the single identifier shared by config, guest and metrics (ADR-004):
// it is stored unprefixed, and the "mws-" prefix is applied when reading either side.
type DiskRef struct {
    DiskID     string // cloud resource name — used for API writes, never written in config
    VMName     string // (VMName, DeviceName) is the fleet-mode key
    DeviceName string // unprefixed: "datadisk"
}

// GuestPath returns /dev/disk/by-id/mws-<DeviceName>.
func (d DiskRef) GuestPath() string
// MetricLabel returns the device_name label value: "mws-<DeviceName>".
func (d DiskRef) MetricLabel() string

type MetricSource interface {
    // Sample returns one observation per managed disk. Units are explicit
    // in the Sample struct; implementations must not mix them.
    Sample(ctx context.Context, disks []DiskRef) ([]Sample, error)
    Kind() SourceKind // guest | platform
}

// Sample is one interval's observation — an average, not a percentile.
// Neither source can honestly give a within-interval p95 at the single-
// sample level: the guest has only diskstats' cumulative ticks/ops (an
// average), and the platform's histogram would need a different read (its
// own bucket approximation) than the guest ever could, which would make
// this field mean two different things depending on Source — exactly the
// silent-mix bug QueueDepth's explicit unit exists to prevent, just for
// latency instead of queue units. The Analyzer (§4.4, US-007) computes the
// actual p95 from a window of these per-interval averages, uniformly for
// both sources.
type Sample struct {
    DiskID       string
    Timestamp    time.Time     // metric timestamp, not wall clock
    ReadOps      float64       // ops/s
    WriteOps     float64       // ops/s
    ReadBytes    float64       // bytes/s
    WriteBytes   float64       // bytes/s
    QueueDepth   QueueDepth    // value + unit (ops | bytes) — never bare
    ReadLatAvg   time.Duration // average latency this interval, not a percentile
    WriteLatAvg  time.Duration
    Source       SourceKind
}

type Analyzer interface {
    Analyze(samples []Sample, p EvaluationPolicy) Analysis
}

type Decider interface {
    Decide(a Analysis, vm VMState, p EffectivePolicy) Decision
}

type Actuator interface {
    Apply(ctx context.Context, d Decision) (Applied, error)
}

type Decision struct {
    DiskID     string
    Action     Action  // ScaleUp | ScaleDown | NoAction | VMCapReached
    Reason     string  // machine-parseable
    Current    int64
    Target     int64
    Demand     float64
    Inputs     Analysis // full evidence, for the journal
}
```

`QueueDepth` carrying its unit in the type is deliberate. Guest gives operations, platform gives
bytes; a silent mix would be a real and hard-to-spot bug.

### 5.3 Configuration

See `docs/configuration.md` for the full model. Disks are addressed by `device_name` and never by
kernel device path (ADR-004). Summary: limits plus disk selection are the user-facing surface, on top
of one built-in default parameter set — no preset name to choose in Phase 1 (ADR-006);
inheritance is `disk → vm → project → platform default` (`fleet`) or `disk → vm → default` (`local`);
advanced controller knobs exist but are opt-in and treated as experimental until telemetry justifies
exposing them.

---

## 6. Non-functional requirements

| # | Requirement | Target | Rationale |
|---|---|---|---|
| NFR-1 | Decision latency after sustained saturation | `local` ≤ 60 s; `fleet` ≤ 5 min | Guest samples at 1–10 s; platform floor is ~135 s (30 s publish + 60 s window + 62–90 s lag) |
| NFR-2 | Reconcile cost | O(disks); one metrics call per project per cycle | Metrics API is per-project, not per-disk |
| NFR-3 | Memory | ≤ 32 MB for 100 disks | Ring buffers are fixed-size; ~360 samples/disk at 10 s over 60 min |
| NFR-4 | Blast radius | One `spec.iops` field, one disk, per write | Nothing else is sent as a value — immutable fields are omitted, not round-tripped (ADR-009) |
| NFR-5 | Safety | Never write outside `[1000, min(10000, 3000×vCPU)]`, always ×100 | Platform enforces disk bounds but **not** the VM cap |
| NFR-6 | Change rate | ≤ `max_changes_per_24h` per disk (default 48) | Runaway guard; billing is per-minute so this is not a cost constraint |
| NFR-7 | Network resilience | Retry with backoff + jitter; idempotency key on every write | Measured: `SSL_ERROR_SYSCALL` and 10–15 s stalls are routine on this network |
| NFR-8 | Timeout | ≥ 30 s, never the SDK's 5 s default | Endpoint discovery alone was measured at 15 s |
| NFR-9 | Cold start | No action until window full **and** baseline established | A fresh agent must not act on partial evidence |
| NFR-10 | Crash safety | Restart loses windows, never doubles a write | Idempotency key + verify-after-write |
| NFR-11 | Availability | Metrics-source outage degrades to no-action, never to a guess | Missing data is not zero load |
| NFR-12 | Permissions | `local` needs no metrics credential at all | Guest source removes the API key requirement |

### 6.1 Failure modes

| Failure | Behaviour |
|---|---|
| Metrics unavailable | Mark disks "no data", take no action, alert after `n` cycles |
| Compute API unavailable | Retry with backoff; decisions queue but never stack up (latest wins) |
| Write returns non-2xx | Log with full request context, back off, do not update state |
| Write succeeds but value differs | Treat as failure, re-read, reconcile on next cycle |
| Concurrent external resize | Discovery picks it up; agent adopts observed value as current |
| VM etag conflict (dependent path) | Abort write, re-read, retry next cycle — never force |
| Agent dies after scale-up | **Disk stays at the raised limit.** No dead-man's switch is possible from outside the platform — documented limitation, must be in the README |

---

## 7. Observability

### 7.1 Exported metrics

```
iops_autoscaler_disk_provisioned_iops{disk,vm,class}          gauge
iops_autoscaler_disk_observed_iops{disk,vm,op}                gauge
iops_autoscaler_disk_utilization_ratio{disk,vm}               gauge
iops_autoscaler_disk_estimated_demand_iops{disk,vm}           gauge
iops_autoscaler_disk_latency_baseline_seconds{disk,vm,op}     gauge
iops_autoscaler_disk_saturated{disk,vm}                       gauge 0|1
iops_autoscaler_decisions_total{action,reason}                counter
iops_autoscaler_writes_total{path,outcome}                    counter   path=disk|vm
iops_autoscaler_write_duration_seconds{path}                  histogram
iops_autoscaler_vm_headroom_iops{vm}                          gauge
iops_autoscaler_vm_cap_reached{vm}                            gauge 0|1
iops_autoscaler_metric_source{disk,source}                    gauge  guest|platform
iops_autoscaler_metric_staleness_seconds{disk}                gauge
iops_autoscaler_window_fill_ratio{disk,direction}             gauge
iops_autoscaler_change_budget_remaining{disk}                 gauge
iops_autoscaler_cost_delta_iops_hours{disk}                   counter
```

`metric_source` and `staleness` are there because ADR-001 accepts two sources: when `local` and
`fleet` disagree, the first question is which source fed the decision and how stale it was.

`cost_delta_iops_hours` accumulates billable IOPS-hours above the free 1000 base, so the value the
autoscaler delivers (or costs) is measurable rather than asserted.

### 7.2 Decision journal

Every reconcile emits one structured record per disk, including no-action:

```json
{
  "ts": "2026-08-28T15:04:05Z",
  "disk": "db-data", "vm": "database-01", "class": "dependent",
  "action": "NoAction",
  "reason": "saturation_gate_not_met",
  "current_iops": 5000,
  "utilization": 0.62,
  "queue_depth": {"value": 0, "unit": "ops"},
  "p95_read_latency_ms": 1.2, "latency_baseline_ms": 1.1,
  "vm_headroom_iops": 4200,
  "window_fill": 1.0,
  "source": "guest"
}
```

This is the primary calibration input for the observe-only stage — thresholds are tuned from these
records, not from reasoning.

### 7.3 Health and alerting

`/healthz` (process alive) and `/readyz` (config valid, discovery succeeded, ≥1 source reachable).

Alert-worthy: metric staleness beyond 3 publication intervals; write failure rate above threshold;
change budget exhausted; `VM_IOPS_CAP_REACHED` sustained (actionable — add vCPU); any disk pinned at
`max_iops` while still saturated (the ceiling is too low).

---

## 8. Testing strategy

### 8.1 Unit tests — the bulk of the value

The pipeline is pure between Discovery and Actuator, so the decision logic is testable with no I/O.

| Target | Cases |
|---|---|
| Config resolver | Inheritance at every level; default-set application; overrides; invalid configs rejected |
| Window/percentile | Empty, partial, exactly-full, wrapped ring; p50/p95/p99 against known series |
| Saturation | Gate passes but no signal → not saturated; each signal alone triggers; sustained vs momentary |
| Demand estimator | Open regime (growing queue); closed regime (latency ratio); clamped at `max_scale_up_ratio` |
| Guardrails | Clamp to `[1000, 10000]`; rounding to 100; `min_change` suppression; ratio limits; cooldown; budget |
| VM headroom | Sibling p95 aggregation; headroom exhausted → `VM_CAP_REACHED`; bursty siblings |
| Classification | `spec` → dependent, `ref` → independent; class change between cycles |
| Units | Guest ops vs platform bytes never silently compared — a mixing attempt must fail to compile or panic in tests |

**Property-based tests** for the invariants that must hold for *any* input:
- output is always in `[min_iops, min(max_iops, 10000, vm_headroom)]`
- output is always a multiple of 100
- `NoAction` whenever the window is not full or the baseline is unset
- scale-down never produces a target above current; scale-up never below

### 8.2 Integration tests — against fakes

An in-process fake MWS API replaying recorded responses (including the real 403/400 bodies captured
during discovery):

- dependent disk write → 403 → agent routes to VM path instead of reporting a permissions error
- `iops` out of bounds → 400 with the platform's actual message
- etag conflict → abort, no write
- idempotency key replay → single effective change
- `503` from the metrics endpoint with an IAM token → agent reports "wrong credential type", not
  "server error"
- injected `SSL_ERROR_SYSCALL` mid-write → retry, exactly one applied change

### 8.3 End-to-end — against the live test environment

The environment built during discovery is the E2E harness: `iops-test-vm` (`base-2-8`, 2 vCPU → 6000
pool), one dependent disk, one independent disk attached by ref.

| Scenario | Method | Assertion |
|---|---|---|
| Scale-up on saturation | `fio randread 4k iodepth=64` on the data disk | IOPS raised within NFR-1; target ≈ `demand/0.85` |
| No scale-up when efficient | `fio --rate_iops` just under limit, shallow queue | **No change** — the ADR-002 property |
| Scale-down after quiet | stop load, wait `scale_down_window` | IOPS lowered, never below `min_iops` |
| No flapping | alternating bursts around the threshold | Changes ≤ expected given hysteresis and cooldown |
| VM cap detection | load both disks simultaneously past 6000 aggregate | `VM_IOPS_CAP_REACHED`, no write |
| Bandwidth-bound case | `fio bs=64k` — pins guest `%util` at 100 and `bandwidth_utilization` at the ceiling while `iops_utilization` reads ~51% | Gate opens on bandwidth (ADR-008); saturation detected via latency/queue, **not** missed |
| Dependent write path | drive the VM-owned disk | Change applied via VM path; all other fields unchanged |
| Independent write path | drive the ref-attached disk | Change applied via disk path |
| Cold start | restart mid-load | No action until window full |
| Dry run | `--dry-run` under load, both classes | Independent: `ValidateOnly` used, server verdict. Dependent: computed verdict only (ADR-007). Zero mutations either way |

The bandwidth-bound scenario is the one that catches a whole class of bugs: a naive IOPS-only
controller sees 513/1000 and concludes there is headroom, while the disk is at 100% utilization with
265 ms write latency and a queue of 135.

### 8.4 Load and soak

- **Fleet scale:** 100 synthetic disks against the fake API — assert NFR-2 (one metrics call per
  project per cycle) and NFR-3 (memory).
- **Soak:** 24 h against the live environment with a diurnal load pattern; assert change count is
  sane, no unbounded memory growth, no budget exhaustion under normal load.
- **Chaos:** random injected API failures at 20% — assert no double-applies and no state divergence.

### 8.5 Calibration harness (observe-only)

Not a test but the mechanism that makes the defaults defensible: run in observe-only against real
workloads, record every decision that *would* have been made plus what the load did next, then tune
thresholds against that corpus. **No default in `docs/configuration.md` should be treated as
validated until it has survived this.**

---

## 9. Implementation plan

### 9.1 Milestones

Product-facing phasing — what ships, in what order, and why — lives in the PRD, §6 "Release Phases".
This table is the engineering sequencing underneath it; the **Phase** column is the mapping. Ordering
rationale (local complete before fleet exists at all; platform metrics validated in `local` before
`fleet` depends on them exclusively) is ADR-005.

| M | Deliverable | Depends on | Phase (PRD §6) |
|---|---|---|---|
| **M0** | Repo skeleton, config schema + resolver, `--validate-config` | — | 1 |
| **M1** | Compute API client (discovery, classification, both write paths) | M0 | 1 |
| **M2** | `MetricSource`: guest implementation + Sampler | M0 | 1 |
| **M3** | Analyzer: windows, percentiles, baselines, saturation | M2 | 1 |
| **M4** | Decider + Guardrails; `--dry-run` via `ValidateOnly` | M1, M3 | 1 |
| **M5** | Observability: metrics, decision journal, health endpoints | M4 | 1 |
| **M6** | **MVP release.** Scale-up actuation enabled (validated live via `--dry-run` first); binary + systemd + `.deb` | M5 | 1 (exit) |
| **M7** | Platform `MetricSource` (`PlatformSource`), usable standalone in `local` mode | M2 | 2 |
| **M8** | Comparison (shadow) mode: `Guest` + `Platform` run concurrently, journaled, divergence surfaced | M6, M7 | 2 (exit) |
| **M9** | Scale-down decision; the default parameter set calibrated from M6–M8 telemetry | M6 | 3 (exit) |
| **M10** | Fleet discovery, project/VM/disk inheritance, container image | M7, M9 | 4 (exit) |
| **M11** | Further fleet capabilities and optimizations | M10 | 5 |

`fleet` mode (M10) is built last and inherits an already-proven `PlatformSource` (M7-M8) and an
already-proven bidirectional `Decider` (M9) — it adds only discovery scope, config inheritance, and
the container deployment shape, nothing decision-related.

### 9.2 Sequencing rationale

**`local` mode ships complete — including bidirectional scaling — before `fleet` mode is built at
all** (M0–M9 precede M10). See ADR-005 for the full comparison against the alternative orderings.
In short: it maximizes value delivered per phase, and it means `fleet` is built on top of a metric
source and a decision engine that have already run against real load, rather than being the first
place either does.

**Scale-up actuation is folded into the MVP itself (M6)**, not gated behind a separate observe-only
release. `--dry-run` (US-013) remains available and is validated against the live test environment
before the flag is flipped to live — that validation is now a step inside M6, not a numbered phase of
its own, because scale-up's blast radius is bounded: it can only add capacity, and any overspend is
visible and reversible on the very next cycle. The estimators in §4.5 are still unvalidated at M6,
but the cost of that being wrong in the *up* direction is bounded in a way it is not in the *down*
direction.

**Scale-down (M9) waits until scale-up has run live.** Its failure mode is real, not just wasteful: a
wrong scale-down throttles the workload. It also gets to reuse M6–M8 telemetry for calibration
instead of guessing thresholds cold.

**Platform metrics (M7) are built and exercised in `local` mode (M8, shadow/comparison against the
trusted guest source) before `fleet` mode exists.** `fleet` has no guest fallback — if the platform
source were validated for the first time there, a bad decision would have no cheaper environment to
have caught it in first.

### 9.3 Definition of done per milestone

Unit tests for new logic; integration tests for new API interactions; the relevant E2E scenario
green; metrics exported for anything new; and the decision journal able to explain any behaviour the
milestone introduces.

### 9.4 Deferred decisions

- **State persistence.** v1 keeps cooldowns and budgets in memory, so a restart forgets them. Bounded
  by the cold-start rule (no action until windows refill), which is at least as long as a cooldown.
  Revisit if restarts prove frequent.
- **Leader election.** Two `fleet` agents on the same project would both act. v1 documents
  "run one"; a lease is the fix if it becomes real.
- **Absolute latency thresholds.** Relative-to-baseline only in v1; absolute mode is in the config
  schema but unimplemented.

---

## 10. Open questions

1. **Resize rate limits.** Undocumented. Determines whether `max_changes_per_24h` (48) is
   conservative or already too high. Test empirically before M8.
2. **`*_quota_utilization` fix.** Committed by the NBS team. When it lands, it becomes the cheap
   utilization gate and removes a `GetDisk` per disk. Note the trap: today `0` means "broken"; after
   the fix `0` legitimately means "idle" (ADR-002).
3. **Operations-denominated queue depth.** Committed. Would let `fleet` use the same saturation logic
   as `local`, narrowing the gap that motivated ADR-001.
4. **Read queue metric.** Absent. Until it exists, read-heavy saturation in `fleet` rests on latency
   alone.
5. **Demand estimator validity.** The core open question. Both regimes in §4.5 are plausible and
   unproven; M6 exists to settle them.
6. **Correlated bursty siblings.** p95 headroom may be too optimistic when several disks burst
   together. Needs multi-disk load data.
7. **Minimum economically meaningful scale-down.** Given the 100-IOPS step and per-minute metering,
   below some delta a change costs more in churn than it saves.
