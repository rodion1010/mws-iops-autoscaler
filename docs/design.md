# MWS IOPS Autoscaler — Design / RFC

**Status:** Draft
**Date:** 2026-08-28
**Author:** generated with Claude Code, reviewed by Product Lead (NBS)

**Derived from `docs/prd-mws-iops-autoscaler.md`, which is the primary document.** The PRD states the
requirements; this document defines the currently accepted architecture for meeting them. It is
normative and living: update it when an architectural decision changes. Track implementation gaps in
issues rather than copying completion status into this document or weakening an accepted invariant.
This document does not introduce, widen or drop a requirement on its own — when a design decision
changes one, the PRD is amended in the same change (see the update rule in `docs/README.md`).

Companion documents: requirements in `docs/prd-mws-iops-autoscaler.md`, configuration model in
`docs/configuration.md`, measured platform behaviour in `docs/platform-reference.md`, and temporary
unresolved decision requests (when needed) in `docs/adr/`.

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
nothing. Only *saturation* — demand that cannot be served — justifies spending more. See §4.4.

---

## 2. Scope

**In scope (v1)**
- Reactive control loop, two modes (`local`, `fleet`).
- Both disk classes: independent via disk API, dependent via VM spec (§4.7).
- One built-in default parameter set as the user-facing surface (limits + opt-in `advanced`);
  named presets deferred past the MVP (configuration.md §1).
- Rollout stages: scale-up-only (with `--dry-run` as the pre-live validation step, not a
  separate release), then bidirectional; fleet follows after local mode is complete (§9.2).

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
| **MetricSource** | One interface, two implementations (guest, platform) | Explicit units on every field (§4.2) |
| **Sampler** | Fixed-size ring buffer of samples per disk | Bounded memory; drops oldest |
| **Analyzer** | Rolling windows, percentiles, latency baselines, saturation verdict | Pure over a sample slice |
| **Decider** | Demand estimate → raw target | Pure; no clamping here |
| **Guardrails** | Clamp, round, rate-limit, cooldown, budget, VM headroom | Last line before any write |
| **Actuator** | Apply the change via the correct write path | Only component that mutates anything |
| **State/Journal** | Last action per disk, cooldowns, 24h budget, decision history | Cooldowns/history in-memory v1; 24h budget persisted as a crash-safe intent/result log (§9.4, issue #10) |

### 3.3 Why this shape

The pipeline is deliberately a chain of **pure functions with I/O at the ends**. Discovery and
Actuator touch the network; everything between them is a pure transformation from
`[]Sample + EffectivePolicy + VMState` to a `Decision`. That makes the entire decision logic testable
with table-driven unit tests and no mocks, which matters because the decision logic is where the
product risk lives.

### 3.4 Architectural invariants

This table is a navigation index, not a second specification. The linked sections are canonical and
contain the complete contracts, edge cases and failure behaviour.

| Area | Invariant | Canonical detail |
|---|---|---|
| Metric acquisition | The mode selects one metric source; every value carries source, unit and freshness metadata. | §4.2, §5.2 |
| Scaling evidence | Utilization only gates evaluation. Scaling requires sustained saturation, and the resource gate checks both IOPS and bandwidth. | §4.4 |
| Latency and queue signals | Latency values retain their derivation; a baseline may be established while saturated but is then frozen; queue values enter IOPS math only when their unit is compatible. | §4.4, §5.2 |
| Scale-down demand | Each aligned sample uses the larger of observed IOPS and bandwidth-equivalent IOPS before the percentile is calculated. | §4.5 |
| Disk identity | `device_name` — the exact value VM status reports — is the stable user-facing selector; long-lived state also includes the platform incarnation ID. | §4.1, §5.2 |
| Mutation paths | Independent and dependent disks use class-specific APIs; writes are minimal partial updates and are conditional on fresh IOPS and incarnation state. | §4.7 |
| Dry run | Independent-disk validation is server-confirmed; dependent-disk validation is explicitly computed-only because the VM API has no validation-only operation. | §4.7 |
| Time model | Sampling has one process-wide cadence; window readiness and gaps are evaluated from timestamps. | §4.2, §4.4 |
| Failure safety | Missing, stale or identity-ambiguous evidence fails closed and invalidates the current reconcile when required. | §6.1, §7.3 |
| Durable state | The 24-hour change budget survives restart; cooldown timestamps may remain in memory. | §9.4 |
| Delivery and defaults | Local mode is completed and calibrated before fleet mode; the MVP exposes one built-in default rather than named presets. | §9.2, `configuration.md` §1 |

---

## 4. Flows

### 4.1 Discovery (slow path, default 5 min)

```
 for each target VM:
   GET  /compute/v1/projects/{p}/virtualMachines/{vm}
   ├─ read spec.storage.disks[]
   │    disk.spec present → DEPENDENT   (write via VM path)
   │    disk.ref  present → INDEPENDENT (write via disk path)
   │    neither/both resolved → ambiguous class, excluded (issue #4)
   ├─ read status.storage.disks[].deviceName
   │    kept verbatim → the config identifier (§4.1)
   ├─ reject a duplicate disk resource identity or device_name shared by two attachments —
   │    both excluded, neither guessed at as "the real one" (issue #4)
   ├─ reject an empty or duplicate alias — the dependent write path's own join key
   │    (write.go's validateDependentWriteTarget); both excluded on a collision (issue #20)
   └─ GET /compute/v1/vmTypes/{vmType}
        vm_iops_limit = 3000 × spec.cpu.vcpuCount
        (spec.disks.totalIops is null on every type — do not use)

 for each disk not already excluded above:
   GET /compute/v1/projects/{p}/disks/{d}
   ├─ spec.iops (current provisioned), status.iops {base, extra, total}
   │    spec.iops <= 0 → invalid current IOPS, excluded (issue #4)
   └─ metadata.id → IncarnationID (docs/platform-reference.md "Disk resource identity") — degrades to
        DiskName-only identity if absent, never excludes the disk (pre-merge review)

 local mode only, for each disk that reached this point:
   resolve /dev/disk/by-id/mws-<name>, verify its kernel device has a /proc/diskstats entry
   (cmd/mws-iops-autoscaler/agent.go's guestPathVerifier — issue #4) — otherwise excluded

 apply selection strategy → managed set
   match config selectors against DeviceName (the exact value VM status reports) —
   never a kernel device path, which is assigned in discovery order and not stable
   across reboots (§4.1)
   fleet: the key is (instance, device_name); deviceName is unique per VM, not globally
 resolve EffectivePolicy per disk
```

Discovery is idempotent and rebuilds the registry wholesale. A disk that changed class, was detached,
or was resized externally is picked up here.

A `disks.selected` or `disks.exclude` entry that doesn't resolve against the VM's live disks
(`registry.Degradation`) is handled differently depending on discovery history, not treated as one
fixed outcome — `registry.Select` itself has no memory of prior cycles (it is rebuilt wholesale every
time, per above) and always returns the managed set it could build plus the unresolved names; deciding
whether that's fatal belongs to `Agent.discover`, which does have history:

- **Cold start** — no discovery has yet succeeded for this process. Managing a silently partial set
  with no prior successful discovery to fall back to would diverge from operator intent with nothing
  to show for it, so this is fatal: the managed snapshot is invalidated exactly as any other discovery
  failure is (`Agent.managedValid`), and nothing is managed until the mismatch is resolved.
- **Runtime** — at least one discovery has already succeeded. Only the unresolved name degrades; every
  other disk keeps being managed normally. The degraded name is reported via
  `health.Readiness.SetDegradedDisks` (§7.3) and the `iops_autoscaler_disk_configured_unmanaged` gauge
  (§7.1) — neither of which affects `/readyz`'s binary verdict — and a `WARN` log line is emitted only
  on the transition into or out of a degraded state, not on every cycle it persists.

No timer escalates a runtime degradation back to fatal; an operator who wants an escalation policy
builds it on top of the exposed signal (docs/TODO.md's alerting entry).

Every exclusion above returns a `compute.SkippedDisk{Alias, DiskName, Category, Reason}` rather than
silently dropping the disk (US-002) — `Category` is one of a small set of stable codes
(`ambiguous_class`, `empty_identity`, `duplicate_disk_name`, `duplicate_device_name`, `empty_alias`,
`duplicate_alias`, `get_disk_failed`, `invalid_iops`, plus `guest_path_unresolvable` from the
local-mode step) suitable as a Prometheus label; `Reason` carries the free-text detail (disk names, the
underlying API error) for
the log line only — `Category` exists separately from `Reason` specifically because `Reason`'s text is
unbounded and would blow up label cardinality if used directly. `telemetry.RecordDiscoverySkipped`
increments `iops_autoscaler_disk_discovery_skipped_total{alias,reason}` for each one (§7.1).

`health.Readiness`'s `source_reachable` (§7.3) additionally requires at least one disk to have survived
every check above: a successful `sampler.Tick()` alone no longer reports the metrics path as usable if
the managed set is empty, since there is nothing left that Tick could possibly have produced a sample
for.

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
`metric_ts` rather than wall clock. Indexing uses `DiskRef.MetricLabel()` — `DeviceName` itself,
already exactly the value the exported `device_name` label uses, so no translation is needed between
the two.

The collection interval is process-wide — one Sampler, one source, one tick — set once by
`runtime.metrics.collection_interval`. There is no per-policy or per-disk override: a single call
already returns every managed disk's sample for that tick, so a disk-level cadence could never
actually be honored (§4.2).

### 4.3 Reconcile (default 30 s)

```
 for each managed disk:
   if resize_in_progress(disk):        skip   ← one in-flight write blocks either direction

   # No blanket top-level "window not full", "baseline not established", or
   # "in cooldown" gate here — each is a per-direction precondition checked
   # inside its own branch below, not a single check before both branches.
   # A blanket gate keyed to one direction's value is wrong for the same
   # reason in all three cases: cooldown.scale_up and cooldown.scale_down
   # are independently configurable (US-012) — a disk in scale-up cooldown
   # only must remain eligible for scale-down, and a blanket skip on either
   # cooldown being active would block the other direction for no reason.
   # (Same principle as §4.2's WindowFullUp/WindowFullDown split and
   # §4.4's baseline fix — a two-direction concept never collapses to
   # one flag.) analyze() reports WindowFullUp/WindowFullDown and
   # BaselineEstablished alongside its other fields; m.Saturated consumes
   # WindowFullUp, m.Underutilized consumes WindowFullDown, each as one
   # AND-term among their own preconditions.

   m  = analyze(disk.id, samples, policy.evaluation, disk.spec_iops)   // m.Saturated / m.Underutilized
                                                                        // already fold in "sustained" (§4.4)
   vm = analyze_vm(sibling disks)

   if m.Saturated:
       if vm_headroom_insufficient(vm, disk):
           emit VM_IOPS_CAP_REACHED ; no write
       elif in_cooldown(disk, up):
           no_action("cooldown_scale_up")
       else:
           target = guardrails(demand(m, policy) / policy.target.scale_up_utilization, disk, vm, up)
           if target > current: actuate(disk, target); start_cooldown(up)
           else: no_action("target_not_above_current")
   elif m.Underutilized:
       if in_cooldown(disk, down):
           no_action("cooldown_scale_down")
       else:
           target = guardrails(m.EffectiveDemandP95Down / policy.target.scale_down_utilization, disk, vm, down)
           if target < current: actuate(disk, target); start_cooldown(down)
           else: no_action("target_not_below_current")
   else:
       no_action(reason)
```

`demand()` is the Decider's job (§3.2 — "Demand estimate → raw target"), not the Analyzer's: it takes
`Analysis` plus policy and produces a number to clamp, same as `m.EffectiveDemandP95Down` already is
for scale-down. `Analysis` carries the *evidence* (queue depth, latency, throttle detection, window/
baseline state, and the two verdict booleans); it does not carry a precomputed demand figure —
keeping that split is what makes `demand()`'s two regimes (§4.5) unit-testable against a fixed
`Analysis` without needing a real window of samples for every case.

`m.EffectiveDemandP95Down`, not `m.IOPSP95Down` alone, sizes the scale-down target (issue #17). NBS-PL2
ties provisioned IOPS and bandwidth at one fixed ratio (`bytesPerIOPS`, §4.4) — a large-block,
low-ops/s workload can be well below its IOPS ceiling while still saturating the bandwidth that same
IOPS number buys. Sizing the target from served ops/s alone would undersize that bandwidth need on the
next cycle. `EffectiveDemandP95Down` is the p95, over `scale_down_window`, of each *sample's own*
`max(ops, bandwidth ÷ bytesPerIOPS)` — not `max(p95(ops), p95(bandwidth))`, which can miss an outlier
that shows up in only one of the two series at any given sample (§4.5 below has the worked example).
`m.IOPSP95Down` and `m.BandwidthP95DownIOPSEquiv` still exist on `Analysis` and are journaled
alongside `m.LimitingResourceDown` (`"iops"`/`"bandwidth"`) so a scale-down decision's journal entry
shows which resource actually drove the target, not just the final number.

`guardrails()` is the last line before any write (§3.2): clamp to `[policy.limits.min_iops,
min(10000, policy.limits.max_iops, vm_headroom)]` (NFR-5, §4.6), round to a 100-IOPS multiple,
apply `max_scale_up_ratio`/`max_scale_down_ratio` relative to `current`, and suppress a change smaller
than `min_change_iops`. The 24h change budget (`max_changes_per_24h`, §9.4) is checked once the
guardrail-clamped target is known to actually differ from `current` — checking it earlier would count
budget against a decision that guardrails might clamp back down to `current` anyway, i.e. a decision
that was never going to write.

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
      window_full_up
  AND resource_gate
  AND ( queue_depth >= policy.saturation.queue_depth_threshold
     OR (baseline_established AND latency.P95 > baseline.P95 × policy.saturation.latency.baseline_multiplier)
     OR throttling_detected )
  AND sustained over policy.evaluation.scale_up_window
```

`window_full_up` is `Analysis.WindowFullUp` — cold start never acts, expressed as one AND-term here
rather than a separate skip line (this section's own reconcile pseudocode above). The latency
disjunct is conditional on `baseline_established` — it evaluates false, never blocks, when no
baseline exists yet. `queue_depth` and `throttling_detected` need no baseline at all and can confirm
saturation on their own (§4.4) — this is what actually breaks the cold-start-while-saturated
deadlock a blanket "skip until baseline exists" gate used to cause.

"Sustained over `scale_up_window`" is not one mechanism — each signal already carries its own notion
of sustain, and none of them collapse to "check only the newest sample":
- `resource_gate` and `queue_depth >= threshold` are point-in-time measures on every `Sample`
  (utilization, `Sample.QueueDepth`) — sustained means every sample whose `Timestamp` falls within the
  trailing `scale_up_window` independently clears the threshold, not just the newest one.
- `latency.P95 > baseline.P95 × multiplier` is already a window-aggregate comparison by construction:
  `latency.P95` is the percentile *over* the trailing `scale_up_window`, so an elevated value already
  reflects sustained, not momentary, latency — there is no separate per-sample re-check to make.
- `throttling_detected` (§4.5: `served_iops ≈ provisioned_limit AND queue_depth sustained > 0`)
  carries sustain in its own definition the same way.

`bandwidth_utilization = (read_bytes/s + write_bytes/s) / (spec.iops × 32768)` — NBS-PL2's fixed
IOPS-to-bandwidth ratio (docs/platform-reference.md: `MB/s = IOPS × 32 / 1024`), using the exact measured bytes/IOPS
figure, not an approximation. `spec.iops` is already read by `Discover` (M1) on the same cycle — no
extra API call.

**Utilization is a gate, not a trigger** (§4.4) — **on either resource** (§4.4). If a disk is
not near either of its provisioned limits it cannot be limit-starved, so the expensive checks are
skipped. A large-block, throughput-heavy workload saturates bandwidth long before it saturates IOPS
count; gating on IOPS alone would silently miss it, so the gate checks both resources.

**Underutilization detection is `saturated`'s mirror, not a separate mechanism** (issue: bandwidth must
gate `Underutilized` too, not only scale-down target sizing — §4.5's `EffectiveDemandP95Down`):

```
resource_quiet :=
      iops_utilization      < policy.saturation.utilization_threshold
  AND bandwidth_utilization < policy.saturation.utilization_threshold

underutilized :=
      window_full_down
  AND resource_quiet
  AND queue_depth < policy.saturation.queue_depth_threshold
  AND (!baseline_established OR latency.P95 <= baseline.P95 × policy.saturation.latency.baseline_multiplier)
  AND !throttling_detected
  AND sustained over policy.evaluation.scale_down_window
```

`resource_quiet` requires **both** resources below threshold, every sample — not merely "not
`resource_gate`" (De Morgan's makes that the much weaker claim that *some* sample had both quiet, not
that *every* sample did; `sustained` here means the latter, same as everywhere else in this section).
Without it, a large-block, low-ops/s workload saturating its bandwidth ceiling could still read as
`underutilized`: `queue_depth`, `latency.P95`, and `throttling_detected` are all IOPS-denominated
signals, blind on their own to a bottleneck that never shows up as an elevated op count. Reusing
`policy.saturation.utilization_threshold` (rather than a new config field) keeps one invariant true in
both directions: a disk can never simultaneously be a `saturated` candidate on one resource (sustained
at-or-above threshold) and an `underutilized` one (sustained below threshold, both resources) at once.

**Latency is baseline-relative, never absolute.** `p95 > 10 ms` is wrong for everyone; acceptable
latency is workload-specific. The baseline is a long rolling low-percentile of the disk's own
observed latency, **frozen against updates** while the disk is saturated so a throttled period cannot
poison an already-established baseline.

**Establishment is not gated by saturation — only updates are** (§4.4). The baseline seeds from
the disk's first full window's low-percentile latency regardless of whether that window happened to
be saturated; the low percentile is exactly the statistic that stays close to true unloaded service
time even under load, which is why a low percentile is used instead of a mean. Only once established does the freeze
apply, holding it fixed for the rest of an active saturation episode. Without this, a disk saturated
since the agent started would never get a quiet sample to learn from — the freeze rule would keep a
never-established baseline unestablished forever, and the latency signal (plus the closed-workload
demand form below) would be permanently unavailable, not just delayed.

**How the p95 is derived is not uniform across `Source`, and is recorded, not assumed** (§5.2).
`guest` always uses `LatencyWindowedAvg` — a percentile estimated from a window of per-interval
averages, the only method `/proc/diskstats` supports. `platform` uses `LatencyHistogram` — a real
quantile computed directly from the exported bucket histogram — whenever a fresh one is available,
falling back to `LatencyWindowedAvg` otherwise (the same present-and-fresh fallback pattern US-005a
already uses for utilization and queue depth). `latency.P95` and `baseline.P95` above are each a
`LatencyStat`, so the `Kind` that produced them travels into `Analysis` and the decision journal —
"why did it scale" can distinguish a histogram-backed verdict from a windowed-average one.

### 4.5 Throttling detection and demand estimation

This is the crux, and it is where `local` mode earns its keep. Throttling detection is the
Analyzer's job (`Analysis.ThrottleDetected`, M3) — it needs nothing beyond the sample window and
`specIOPS`. Demand estimation is the Decider's (`demand()`, M4): it consumes `Analysis` (queue depth,
latency, `ThrottleDetected`) plus policy to produce the figure `§4.3`'s `target = guardrails(demand(m,
policy) / policy.target.scale_up_utilization)` clamps.

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
| Closed workload (bounded concurrency) | queue **stable and deep** | `demand ≈ served × (latency.P95 / baseline.P95)` |

The closed-workload form is the workhorse: if latency is 3× baseline, the workload is receiving
roughly a third of the service it wants. Both forms are clamped to
`max_scale_up_ratio` so a pathological reading cannot request a 10× jump.

`dL/dt` (`Analysis.QueueGrowthRate`) is only summed with `served` (ops/s) when
`Analysis.QueueGrowthRateValid` is true — the Analyzer sets it false whenever the up-window's
`QueueDepth` is not entirely `local`'s own ops unit (issue #18), since `fleet`'s queue metric is
byte-denominated (docs/platform-reference.md) and `served + dL/dt` is only dimensionally valid for ops/s. An invalid
growth rate falls through to the closed-workload form exactly as a growth rate that's simply not above
`queueGrowthThreshold` does — never a raw bytes/s number added onto an ops/s figure, however large
that number is. Today `local` (guest) is the only implemented source and always produces the ops unit,
so `QueueGrowthRateValid` is unconditionally true through the real pipeline; the guard exists so a
future `fleet` (platform) window can never silently reach this formula.

The closed-workload form needs `baseline_established` (§4.4); the open-workload form does not — a
growing queue is visible without any latency baseline at all. In the one corner where saturation was
already confirmed via `queue_depth` or `throttling_detected`, the workload is closed, and the
baseline has not yet been computed from this window, `demand()` has no ratio to compute and the cycle
emits `NoAction` with that specific reason rather than guessing. Because the baseline seeds from this
same window regardless of saturation (§4.4), this is a single reconcile cycle, not a repeat of the
deadlock the blanket gate used to cause — the next cycle has a baseline to work with.

**Platform-side (fleet mode).** No queue-in-operations and no read queue, so only the
latency-ratio form is available — but that ratio itself can use `LatencyHistogram`, the more accurate
of the two `LatencyStatKind`s (§5.2), when a fresh histogram is present. The form being unavailable
without a queue signal is what makes this coarser than guest's throttling detection, and one more
reason `local` prefers the guest (§4.2) — it is not a statement about the latency figure's own
accuracy.

**Scale-down sizing is bandwidth-aware too (issue #17).** The scale-up estimators above size a target
from *served* activity when the disk is pinned at its own limit; scale-down sizing (§4.3) is simpler —
`EffectiveDemandP95Down` is already an unthrottled p95 read straight off the down-window, no
regime-detection needed — but it shares the same NBS-PL2 fact: provisioned IOPS and bandwidth move
together at a fixed ratio (`bytesPerIOPS`, §4.4), so a target sized from ops/s alone can undersize
the bandwidth a large-block workload needs. The Analyzer computes, per sample in the down-window, both
`ops = read_ops + write_ops` and `bandwidth_iops_equiv = (read_bytes + write_bytes) / bytesPerIOPS`,
takes `max(ops, bandwidth_iops_equiv)` **per sample**, then takes the p95 of *that* series —
`EffectiveDemandP95Down`. This is deliberately not `max(p95(ops), p95(bandwidth_iops_equiv))`: two
percentiles computed independently can each land on a different sample, so their max can miss an
outlier that only ever appears in one series at a time (e.g. one sample with an ops spike and a
different sample with a bandwidth spike, neither reaching p95 on its own but each mattering when it
occurs). Aligning the max per sample before taking the percentile is what makes the outlier visible.
`IOPSP95Down` and `BandwidthP95DownIOPSEquiv` are still computed and carried on `Analysis` — and
journaled, with `LimitingResourceDown` — purely for observability into which resource actually bound.

**Honesty note.** These estimators are unvalidated. They are why `--dry-run` (US-013) is run against
the live test environment as a validation step inside M6 before actuation is flipped on — not a
separate observe-only release gating the MVP (see the delivery sequence in §9.2) — checking
whether the estimate matches what actually happened after a manual resize before the estimator is
ever allowed to act on its own.

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

"Other disks" means every disk on the VM, managed or not — including one dropped from the managed
set this cycle because its guest path could not be resolved (§4.1). A sibling with no fresh sample
this cycle contributes `IOPSP95Down`'s zero value to the aggregate, which understates its real
consumption rather than reporting it as unknown. Scale-up must fail closed on that gap: if any
sibling sharing the pool has no data or a stale sample this cycle, the controller takes no action
(reason `sibling_demand_unmeasurable`) instead of approving a resize against headroom that might
already be spent. This is deliberately **not** `VM_IOPS_CAP_REACHED` — that action drives
`iops_autoscaler_vm_cap_reached`, alert-worthy as "actionable — add vCPU" (§7.3), and a measurement
gap is a different remediation (fix that disk's metrics path) that must not alias onto the vCPU-upgrade
signal. Scale-down is unaffected — reducing this disk's own IOPS never consumes more of the shared
pool, so an unmeasured sibling has nothing to fail closed against on that path (issue #56).

### 4.7 Actuation

**Both write paths are conditional, not unconditional (issue #16).** A Decision's `Target` is only
meaningful relative to the `Current` it was computed from — every guardrail in §4.5/§4.6 clamps
around that specific value. If the live value has moved since the Decision's snapshot was taken (an
operator or another controller changed it, or the agent's own previous write already did), applying
`Target` as an absolute value can silently undo an external increase or, in the other direction,
silently re-widen a value someone had deliberately narrowed. So immediately before every write, both
paths re-read the live resource, and abort — never force — if what they find doesn't match what the
Decision expected:

```
if live.current != decision.current:                 abort (ErrConcurrentModification)
if target crosses live.current in the wrong direction: abort (ErrConcurrentModification)  ← defensive,
                                                         redundant with the check above in the
                                                         ordinary case, kept independent so it still
                                                         holds if that check is ever loosened
```

The re-read's own etag is what actually travels on the write — never one carried from an earlier
Discover — so the optimistic-concurrency precondition compares against state no older than the write
itself. On `ErrConcurrentModification` the orchestrator (`Agent.actuate`) forces an immediate
rediscovery rather than waiting for the next scheduled `DiscoveryInterval` tick, so the next reconcile
cycle decides again from a snapshot no older than the conflict — never a forced retry of the stale
target. A successful write's verified applied value is adopted into the Agent's own current-value
snapshot immediately, for the same reason: waiting for the next discovery would leave it stale for up
to `DiscoveryInterval`, the exact class of gap this issue exists to close. See §4.7.

**The forced rediscovery also ends the current reconcile cycle, not just the current disk's write
(pre-merge review).** `Agent.reconcile` snapshots `managed`/`iopsPool` once at the top of the cycle —
every sibling p95, VM headroom, and Decision for every disk not yet processed is computed from that one
snapshot. A rediscovery mid-cycle replaces it, which means every one of those not-yet-processed
decisions was computed from data the platform has already moved on from. `Agent.generation` (bumped
only alongside a successful discovery, in the same critical section as `managed`/`iopsPool`) is
snapshotted the same way at the top of `reconcile`; the per-disk loop compares it again after every
`actuate()` call and returns immediately if it has changed, leaving every remaining disk for the next
reconcile tick to decide fresh. `vm_cap_reached` is not published for an aborted cycle either — an
`anyVMCapReached` computed from only part of the managed set would be a possible false negative, no
better than not publishing it at all.

**Independent disk** — a pre-write `GetDisk` for the fresh etag/live-current check above, then a
single-field partial update, still no read-modify-write of the write body itself:

```
GET  compute.mwsapis.ru/compute/v1/projects/{p}/disks/{d}     capture etag, live spec.iops
POST compute.mwsapis.ru/compute/v1/projects/{p}/disks/{d}?updateOnly=true
Idempotency-Key: <uuid>
{"metadata": {"etag": "..."}, "spec": {"iops": N}}
```

`validateOnly` (US-013's dry run) still exercises every check above — including the live-current
comparison — and only skips the final mutating call; a dry run therefore reports a would-be conflict
exactly like a real run would (issue #16's "dry-run exercises the same precondition checks without
mutation").

**Dependent disk** — read-modify-write through the VM, which is the riskier path:

```
GET  .../virtualMachines/{vm}                   capture etag, every disk's alias (name)
  build a minimal partial body: every alias, in order, carrying nothing but its name —
  except the target alias, which additionally carries spec.iops
POST .../virtualMachines/{vm}                   send etag, Idempotency-Key
```

Before any of the above, `validateDependentWriteTarget` (issue #20) proves the fresh read is safe to
act on at all: every alias non-empty and unique across the whole VM, and exactly one attachment
matching the requested `device_name` — a descriptive error aborts before `UpdateDependentDisk` is ever
called if either fails. The platform should already guarantee both, but this write path does not trust
that blindly: `Upsert`'s delete-on-omission semantics (next paragraph) make an empty or duplicated
alias a malformed collection, not a harmless duplicate, and more than one attachment sharing
`device_name` previously let the target-selection loop silently pick whichever it saw last — an
arbitrary attachment, not necessarily the intended one. Discovery applies the same alias checks
(§4.1) so a colliding disk is excluded from the managed set before it is ever offered as a target, but
the write path's own check is what actually protects the write — Discovery's snapshot can be stale by
up to `discovery_interval`; this check runs against the same fresh read the write itself uses.

Every alias must still be present, because Upsert treats an existing `storage.disks[]` element
missing from the request as a delete — dropping an entry to "only touch the target" would detach
every other disk on the VM (issue #3). But no entry, including the target, carries anything beyond
its collection key (`name`) plus the one field actually changing. Earlier drafts of this path
round-tripped a full disk spec — `boot`, `deviceName`, `diskType`, `size` — reconstructed from the
GET response and resent as an assertion that nothing else had changed. That is strictly worse than
omitting those fields: `name`/`boot`/`deviceName`/`diskType` are immutable on update and the API
accepts them omitted entirely (docs/platform-reference.md "DiskSpecRequest mutability"), so a value resent through a
lossy response→domain→request translation risks a mismatch the platform would reject or, worse,
silently accept as an unintended change. Sending the collection key alone carries the same
concurrency guarantee (the etag) without manufacturing a value for a field the write never intended
to touch.

`UpdateVirtualMachine` has no `validateOnly` — confirmed against the generated SDK client, which has
no such field on this request (unlike `UpdateDiskRequest`, which does). **This path has no
server-side dry-run** (§4.7): `--dry-run` can report the computed decision and the agent's own
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
// DeviceName is the single identifier shared by config, guest and metrics
// (§4.1): it is the exact value status.storage.disks[].deviceName reports,
// and the exact value config selectors must match.
type DiskRef struct {
    DiskID     string // cloud resource name — used for API writes, never written in config
    VMName     string // (VMName, DeviceName) is the fleet-mode key
    DeviceName string // the exact value VM status reports: "mws-datadisk"

    // IncarnationID is compute.StorageDisk.IncarnationID (the platform's
    // own metadata.id, docs/platform-reference.md "Disk resource identity"), carried here
    // so the Sampler's ring buffer can be keyed by IdentityKey() instead
    // of DiskID alone (pre-merge review — see that method's own comment).
    IncarnationID string
}

// GuestPath returns /dev/disk/by-id/<DeviceName>.
func (d DiskRef) GuestPath() string
// MetricLabel returns the device_name label value: DeviceName itself.
func (d DiskRef) MetricLabel() string
// IdentityKey returns DiskID + IncarnationID, combined — the Sampler's
// own ring-buffer key, and (via compute.StorageDisk's identical method)
// the key Tracker/Budget use for cooldown and the 24h change budget.
// DiskID alone is not safe for any of these: it is the disk's name, which
// is user-settable for an independent disk and reusable after deletion —
// a disk deleted and recreated under the identical name must start every
// one of these systems fresh, not inherit the previous incarnation's.
func (d DiskRef) IdentityKey() string

type MetricSource interface {
    // Sample returns one observation per managed disk. Units are explicit
    // in the Sample struct; implementations must not mix them.
    Sample(ctx context.Context, disks []DiskRef) ([]Sample, error)
    Kind() SourceKind // guest | platform
}

// Sample is one interval's observation — an average, not a percentile.
// Neither source can honestly give a within-interval percentile at the
// single-sample level: guest has only diskstats' cumulative ticks/ops (an
// average is all it can ever produce), and platform's real distribution
// lives in its exported histogram, not in any one Sample. The Analyzer
// (§4.4, US-007) turns these into a percentile over a window — see
// LatencyStat below for why that computation is not uniform across
// Source (§5.2).
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

// LatencyStatKind names how a latency percentile was derived — carried on
// the value itself, the same move QueueDepth makes for its unit, so a
// windowed estimate and a histogram-derived quantile are never silently
// compared as if they meant the same thing (§5.2).
type LatencyStatKind string

const (
    // LatencyWindowedAvg: a percentile estimated from a window of
    // per-interval Sample averages. The only method guest can ever
    // produce; platform's fallback when no histogram is available.
    LatencyWindowedAvg LatencyStatKind = "windowed_avg"
    // LatencyHistogram: a real quantile computed directly from platform's
    // exported bucket histogram (docs/platform-reference.md) for a single scrape — no
    // windowing needed for accuracy. platform only.
    LatencyHistogram LatencyStatKind = "histogram"
)

type LatencyStat struct {
    P95  time.Duration
    Kind LatencyStatKind
}

// Analysis is the Analyzer's output: the evidence and verdict the Decider
// and the journal need, computed once per disk per cycle. Saturated and
// Underutilized are the "saturation verdict" §3.2 assigns to the Analyzer —
// each already folds in its own "sustained over window" evaluation (§4.4),
// so the reconcile loop's `if saturated(m, policy)` is just `if
// m.Saturated`. Everything else on Analysis is the evidence *behind* that
// verdict, for the journal and for the Decider's demand estimate (§4.5) —
// Analysis itself carries no demand figure ("Demand estimate → raw target"
// is the Decider's job, §3.2), so the two regimes in §4.5 stay
// unit-testable against a fixed Analysis without a real sample window for
// every case. ReadLatency/WriteLatency/Baseline carry LatencyWindowedAvg
// today, always — M3 has no histogram source to read (Sample carries no
// histogram data; that arrives with PlatformSource in M7). Baseline is
// frozen against updates while the disk is saturated (§4.4) —
// BaselineEstablished is a separate state question (do we have one yet)
// from Baseline.Kind (how it was computed); establishing it is never
// blocked by saturation, only revising it is (§4.4).
type Analysis struct {
    DiskID string

    // Fresh is false when the newest buffered sample is older than
    // EvaluationPolicy.MaxSampleAge — sampling has stopped or fallen badly
    // behind (issue #15). Saturated and Underutilized are both
    // unconditionally false whenever Fresh is false: a window whose
    // *span* has been full for hours reads as "full" forever even after
    // sampling silently stops, since nothing inside a fixed buffer alone
    // can reveal that time has kept passing outside it — only a
    // comparison against wall clock (Analyze's own `now` parameter,
    // below) can. SampleAge is exposed alongside Fresh for the journal and
    // for telemetry.RecordStaleness, not just as an internal gate.
    Fresh     bool
    SampleAge time.Duration

    // Latest-sample snapshot, for the journal — not what Saturated/
    // Underutilized are computed from (each re-evaluates its own signal
    // against every sample in its own window, not just this one).
    IOPSUtilization      float64
    BandwidthUtilization float64
    QueueDepth           QueueDepth
    ReadLatency          LatencyStat // p95 over scale_up_window
    WriteLatency         LatencyStat
    ThrottleDetected     bool

    Baseline            LatencyStat // low percentile over the full buffered history
    BaselineEstablished bool

    WindowFullUp   bool // span ≥ scale_up_window (§4.2)
    WindowFullDown bool // span ≥ scale_down_window

    Saturated     bool // resource_gate AND (queue/latency/throttle) AND sustained (§4.4); false if !WindowFullUp
    Underutilized bool // resource_quiet AND every other signal quiet AND sustained (§4.4); false if !WindowFullDown

    // IOPSP95Down/BandwidthP95DownIOPSEquiv/LimitingResourceDown are the
    // down-window demand breakdown, for the journal (issue #17).
    // EffectiveDemandP95Down — not IOPSP95Down alone — is the Decider's
    // §4.3 scale-down sizing input: the p95, over scale_down_window, of
    // each sample's own max(ops, bandwidth ÷ bytesPerIOPS), not
    // max(p95(ops), p95(bandwidth)) (§4.5 explains why that distinction
    // matters). LimitingResourceDown is "iops" or "bandwidth", whichever
    // of the first two is larger.
    IOPSP95Down                float64
    BandwidthP95DownIOPSEquiv  float64
    EffectiveDemandP95Down     float64
    LimitingResourceDown       string

    // demand()'s (§4.5) two remaining inputs beyond what's above — both
    // measured over ScaleUpWindow, since demand() is only ever called from
    // the Saturated branch. ServedIOPS is exposed directly (not just
    // IOPSUtilization, a ratio) so the Decider never needs specIOPS a
    // second time. QueueGrowthRate is (newest−oldest queue depth) / elapsed
    // seconds within the up-window. It must NOT be assumed to sum directly
    // with ServedIOPS (issue #18) — QueueGrowthRateValid is true only when
    // every up-window sample carried QueueDepthOps; the Decider checks it
    // before using the open-workload formula, falling back to the
    // closed-workload one otherwise.
    ServedIOPS           float64
    QueueGrowthRate      float64
    QueueGrowthRateValid bool
}

type Analyzer interface {
    // diskID is explicit rather than read from samples[0].DiskID — the
    // empty-window case (§8.1's own test matrix: "Empty, partial, exactly-
    // full...") has no samples to read it from, and Analysis.DiskID must
    // still identify which disk produced a "no data yet" result.
    // specIOPS is the disk's current provisioned limit (Discover, M1) —
    // needed for iops_utilization, bandwidth_utilization (§4.4), and
    // throttling_detected's "served ≈ provisioned_limit" comparison (§4.5).
    // It is live disk state, not a policy value, so it is not part of
    // EvaluationPolicy.
    // now is wall clock, explicit for the same testability reason
    // Decider.Decide takes it — Analysis.Fresh/SampleAge (issue #15) are a
    // comparison against real time passing, not against anything
    // derivable from the samples themselves.
    Analyze(diskID string, samples []Sample, p EvaluationPolicy, specIOPS int64, now time.Time) Analysis
}

// EvaluationPolicy gained two freshness/gap bounds for issue #15 (and
// issue #7's "gap-aware window coverage"), both derived by the
// orchestrator from runtime.collection_interval rather than being
// per-disk policy knobs (main.go: 3x for MaxSampleAge, 2x for
// MaxSampleGap — calibration-pending, §8.5):
//   MaxSampleAge time.Duration // newest sample must be no older than this for Fresh
//   MaxSampleGap time.Duration // no two consecutive samples inside a window may be farther apart than this
// windowFull (§4.3/§4.2) now checks MaxSampleGap between every
// consecutive pair of samples in the window, not just overall span —
// a window whose span covers the target duration but contains one large
// silent gap (a missed collection cycle, a temporary source outage) no
// longer reads as "full."

// VMState is the sibling-disk headroom picture (§4.6) — pre-aggregated by
// the caller (analyze_vm in §4.3's pseudocode, not this package): IOPSPool
// and every other managed disk's own Analysis are already available from
// the same reconcile cycle, so recomputing them inside Decide would just
// be redoing work the caller already did.
type VMState struct {
    IOPSPool       int64   // 3000 × vCPU (Discover, M1) — not enforced by the platform
    SiblingP95IOPS float64 // aggregate p95 IOPS of every other disk sharing the VM's pool

    // SiblingDemandUnknown is true when at least one such disk had no fresh
    // sample this cycle — its real consumption was folded into
    // SiblingP95IOPS as zero rather than reported as unknown (§4.6). Decide
    // fails closed on scale-up when this is true (issue #56).
    SiblingDemandUnknown bool
}

// EffectivePolicy is the narrow slice of the resolved policy Decide needs —
// a plain struct decoupled from internal/config, the same way
// analyzer.EvaluationPolicy is (design.md §3.3); translating a resolved
// policy into this shape is the orchestrator's job.
type EffectivePolicy struct {
    MinIOPS int64
    MaxIOPS int64

    ScaleUpUtilization   float64
    ScaleDownUtilization float64

    CooldownScaleUp   time.Duration
    CooldownScaleDown time.Duration

    MinChangeIOPS     int64
    MaxScaleUpRatio   float64
    MaxScaleDownRatio float64

    MaxChangesPer24h int
}

// GuardrailState is cooldown/budget state as of now, resolved by the
// caller from wherever it is tracked (§9.4: the 24h counter is
// persisted, cooldown timestamps are not) — Decide reads it, never owns
// it, keeping this package pure like Analyzer (§3.3). ResizeInProgress
// covers both directions: one in-flight write blocks either.
type GuardrailState struct {
    ResizeInProgress bool
    LastChangeUp     time.Time // zero value = never
    LastChangeDown   time.Time
    ChangesInLast24h int
}

// state.Tracker.State reports ChangesInLast24h as a large sentinel
// (effectively unbounded) once the durable budget has become unhealthy —
// any persistence operation (Reserve/Confirm/Release) failing mid-run
// (issue #10) — rather than adding a second "refuse to act" signal
// GuardrailState/Decide would need to know about. The existing
// `ChangesInLast24h >= p.MaxChangesPer24h` → budget_exhausted branch
// (§4.6 below) already does the right thing unmodified.

type Decider interface {
    // diskID/current mirror Analyzer.Analyze's explicit-diskID rationale —
    // Decision.DiskID/Current must be set even on an Analysis with no
    // samples. now is explicit so cooldown math is testable without a real
    // clock (the same reason GuestSource takes an injectable now func).
    Decide(diskID string, current int64, a Analysis, vm VMState, p EffectivePolicy, state GuardrailState, now time.Time) Decision
}

// PolicyDecider (the concrete implementation) also takes allowScaleDown, a
// constructor-only capability, not a per-call argument: Phase 1 (PRD §6) is
// scale-up only, and that must not depend on any user-tunable threshold
// happening to never trigger scale-down (issue #14). With allowScaleDown
// false, Underutilized still computes and surfaces a candidate Demand for
// observability, but Action is always no_action/scale_down_disabled,
// checked before cooldown or guardrails — never merely clamped by them.
// The orchestrator applies the same capability a second time, independently,
// before ever calling the Actuator (defense in depth: a Decider is an
// interface, and the caller must not simply trust an implementation to have
// gotten this right). Phase 3 flips this one constructor argument and
// nothing else.

// Decide's default (neither Saturated nor Underutilized) branch reports
// four distinct NoAction reasons, not one generic "no_signal" (issue #15,
// deepened by the pre-merge review) — an operator reading the
// journal/metrics needs to tell "this disk has never produced a single
// sample" apart from "this disk's data can't be trusted right now" apart
// from "this disk hasn't accumulated enough history yet" apart from "this
// disk is genuinely, trustworthily idle". Order matters: !a.HasData is
// checked first, because a.Fresh is also false (zero value) for a disk
// that has never been sampled — without that ordering, true cold-start
// would misreport as metrics_stale, implying data once flowed and has
// since gone stale, a different operational fact (and a different fix)
// than never having received any data at all.
//   no_data          — !a.HasData (never sampled — true cold start)
//   metrics_stale    — a.HasData but !a.Fresh (sampled before, now too old)
//   window_not_full  — a.Fresh but neither WindowFullUp nor WindowFullDown
//   no_signal        — a.Fresh and at least one window full, nothing triggered
//
// A fifth reason, discovery_stale, is not one of Decide's own outcomes —
// it is reported by the orchestrator (agent.go's reconcile) when a cycle
// never reaches Decide at all, because the discovery snapshot itself is
// untrusted (invalidated by a fatal discover() error, or older than
// max_discovery_age; §7.3/§4.7). Every disk still sitting in that
// untrusted managed set gets one NoAction/discovery_stale journal record
// per skipped cycle (design.md §7.2/§4.3's own rule: "why didn't it
// scale? must always be answerable from the log alone" — a cycle skipped
// for stale discovery must not read as if nothing happened at all).

type Actuator interface {
    Apply(ctx context.Context, d Decision) (Applied, error)
}

type Decision struct {
    DiskID  string
    Action  Action // scale_up | scale_down | no_action | vm_cap_reached — snake_case,
                    // same convention as Reason, since both feed the same machine-parseable journal (§7.2)
    Reason  string // machine-parseable
    Current int64
    Target  int64  // == Current when Action is no_action or vm_cap_reached
    Demand  float64
    Inputs  Analysis // full evidence, for the journal

    // Carried forward from vm/state (§4.6, §9.4) so a journal/metrics
    // consumer (M5) doesn't need those inputs threaded through separately
    // just to report on them — Decide already computed/received them.
    VMHeadroomIOPS  int64
    BudgetRemaining int
}
```

`guardrails()` — folded into `Decide`, not a separate interface; the architecture diagram's separate
Guardrails box (§3.2) is an internal step of `Decide`, not a second pluggable component — clamps to
`[p.MinIOPS, min(10000, p.MaxIOPS, vm.IOPSPool − vm.SiblingP95IOPS)]` (NFR-5, §4.6), rounds to a
100-IOPS multiple, applies `p.MaxScaleUpRatio`/`p.MaxScaleDownRatio` relative to `current`, and
suppresses a change smaller than `p.MinChangeIOPS`. `state.ChangesInLast24h >= p.MaxChangesPer24h` is
checked only once the guardrail-clamped target is known to actually differ from `current` — checking
budget on a decision guardrails would clamp back to `current` anyway would count against a change that
was never going to write.

`demand()` (§4.5), only ever called from the Saturated branch:

```
regime  := a.QueueGrowthRate > queueGrowingThreshold ? open : closed

demand  := open:   a.ServedIOPS + a.QueueGrowthRate
           closed: a.BaselineEstablished
                       ? a.ServedIOPS × (max(a.ReadLatency.P95, a.WriteLatency.P95) / a.Baseline.P95)
                       : unavailable → NoAction("baseline_unavailable")
```

The closed-regime `unavailable` branch is defensive, not reachable through the real pipeline: `Analyze`
(M3) always sets `BaselineEstablished` the same cycle `WindowFullUp` first becomes true, and
`Saturated` already requires `WindowFullUp` — so by construction, `Saturated ⟹ BaselineEstablished`.
The "single reconcile cycle of `NoAction`" scenario in §4.4 is handled by that guarantee before `demand`
is ever called, not inside it; the branch stays only because `Decide` must not divide by zero if ever
handed an `Analysis` that violates the invariant (e.g. a hand-built one in a Decider-only unit test).

`QueueDepth` carrying its unit in the type is deliberate. Guest gives operations, platform gives
bytes; a silent mix would be a real and hard-to-spot bug.

### 5.3 Configuration

See `docs/configuration.md` for the full model. Disks are addressed by `device_name` and never by
kernel device path (§4.1). Summary: limits plus disk selection are the user-facing surface, on top
of one built-in default parameter set — no preset name to choose in Phase 1 (configuration.md §1);
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
| NFR-4 | Blast radius | One `spec.iops` field, one disk, per write | Nothing else is sent as a value — immutable fields are omitted, not round-tripped (§4.7) |
| NFR-5 | Safety | Never write outside `[1000, min(10000, 3000×vCPU)]`, always ×100 | Platform enforces disk bounds but **not** the VM cap |
| NFR-6 | Change rate | ≤ `max_changes_per_24h` per disk (default 48) | Runaway guard; billing is per-minute so this is not a cost constraint |
| NFR-7 | Network resilience | Retry with backoff + jitter; idempotency key on every write | Measured: `SSL_ERROR_SYSCALL` and 10–15 s stalls are routine on this network |
| NFR-8 | Timeout | ≥ 30 s, never the SDK's 5 s default | Endpoint discovery alone was measured at 15 s |
| NFR-9 | Cold start | No action until window full; no action on a signal-baseline pairing whose baseline is not yet established (queue/throttling-based signals are unaffected) | A fresh agent must not act on partial evidence — but must not deadlock on an already-saturated disk either (§4.4) |
| NFR-10 | Crash safety | Restart loses windows, never doubles a write | Idempotency key + verify-after-write |
| NFR-11 | Availability | Metrics-source outage degrades to no-action, never to a guess | Missing data is not zero load |
| NFR-12 | Permissions | `local` needs no metrics credential at all | Guest source removes the API key requirement |

### 6.1 Failure modes

| Failure | Behaviour |
|---|---|
| Metrics unavailable | Mark disks "no data" — reason `no_data` if never sampled at all (`Analysis.HasData = false`), `metrics_stale` if sampled before but now too old (`Analysis.Fresh = false`, issue #15, pre-merge review) — take no action, alert after `n` cycles |
| Compute API unavailable | Retry with backoff; decisions queue but never stack up (latest wins). Once the last successful discovery exceeds `max_discovery_age` (3× `discovery_interval`), reconcile skips every disk for that cycle rather than keep acting on a snapshot that may no longer reflect reality (issue #15) |
| Discovery fails outright (Compute API error, or an unmatched `disks.selected`/`disks.exclude` name at cold start, issue #5 reopened) | The managed snapshot is invalidated the instant the failure happens (`Agent.managedValid`), not merely once `max_discovery_age` eventually elapses — a fatal error and staleness-by-age are two independent, both-checked gates (pre-merge review) |
| An unmatched `disks.selected`/`disks.exclude` name at runtime (≥1 prior successful discovery, §4.1) | Not fatal — only that name degrades (`registry.Degradation`); every other disk keeps being managed, reported via `/readyz`'s body and a gauge, never the binary verdict |
| Write returns non-2xx | Log with full request context, back off, do not update state |
| Write succeeds but value differs | Treat as failure, re-read, reconcile on next cycle |
| Write call itself errors (network) | Reservation resolved from a fresh live read, never released on the error alone — the platform may have applied it before the response arrived (pre-merge review; see §9.4's budget crash-safety note) |
| Concurrent external resize | Discovery picks it up; agent adopts observed value as current |
| VM etag conflict (dependent path) | Abort write, re-read, retry next cycle — never force |
| Concurrent-modification conflict mid-cycle | The forced rediscovery it triggers (below) supersedes every decision this reconcile cycle already made from the old snapshot — the rest of the cycle aborts rather than acting on them (pre-merge review) |
| Agent dies after scale-up | **Disk stays at the raised limit.** No dead-man's switch is possible from outside the platform — documented limitation, must be in the README |

---

## 7. Observability

### 7.1 Exported metrics

```
iops_autoscaler_disk_provisioned_iops{disk,vm,class}          gauge
iops_autoscaler_disk_proposed_target_iops{disk,vm,class}      gauge
iops_autoscaler_disk_observed_iops{disk,vm,op}                gauge
iops_autoscaler_disk_utilization_ratio{disk,vm}               gauge
iops_autoscaler_disk_estimated_demand_iops{disk,vm}           gauge
iops_autoscaler_disk_latency_baseline_seconds{disk,vm}        gauge
iops_autoscaler_disk_saturated{disk,vm}                       gauge 0|1
iops_autoscaler_decisions_total{action,reason}                counter
iops_autoscaler_writes_total{path,outcome}                    counter   path=disk|vm
iops_autoscaler_write_duration_seconds{path}                  histogram
iops_autoscaler_vm_headroom_iops{disk,vm}                     gauge
iops_autoscaler_vm_cap_reached{vm}                            gauge 0|1
iops_autoscaler_vm_min_iops_floor_exceeds_pool{vm}             gauge 0|1
iops_autoscaler_metric_source{disk,source}                    gauge  guest|platform
iops_autoscaler_metric_staleness_seconds{disk}                gauge
iops_autoscaler_window_fill_ratio{disk,direction}             gauge
iops_autoscaler_change_budget_remaining{disk}                 gauge
iops_autoscaler_cost_delta_iops_hours{disk}                   counter
iops_autoscaler_disk_discovery_skipped_total{alias,reason}    counter
iops_autoscaler_disk_configured_unmanaged{device_name,reason} gauge 1, deleted when resolved
iops_autoscaler_budget_healthy                                gauge 0|1
```

`disk_provisioned_iops` and `disk_proposed_target_iops` are deliberately separate series (issue #19):
`RecordDecision` sets `disk_provisioned_iops` from `Decision.Current` — the value actually in effect —
and `disk_proposed_target_iops` from `Decision.Target`, unconditionally, on every cycle including
`NoAction` (where the two are equal). `disk_provisioned_iops` only ever moves again once a write is
*verified* to have landed: `RecordAppliedIOPS`, called from `actuate()` after Actuator confirms the
applied value (never for a dry run or a failed write), is the only other writer of that gauge. Before
this split, `RecordDecision` set `disk_provisioned_iops` to `Decision.Target` directly — reporting a
disk as already resized before `actuate()` had even run, and leaving it that way forever on a dry run
or a failed write.

`vm_headroom_iops` carries a `disk` label — the value (`Decision.VMHeadroomIOPS`, §4.6:
`vm_iops_limit − p95_aggregate_iops_of_other_disks`) is inherently disk-relative, since "other disks"
depends on which disk it's computed for; two disks on the same VM generally see different headroom.
Reporting it under `{vm}` alone (pre-issue-#19) made whichever disk reconcile processed last silently
overwrite every other disk's own figure.

`vm_cap_reached` stays `{vm}`-only — unlike headroom, this really is one VM-wide fact — but is now set
exactly once per reconcile cycle, via a separate `RecordVMCapReached(vmName, anyCapped)` call after
every managed disk on that VM has been decided, where `anyCapped` is the OR across all of them.
Previously `RecordDecision` set it per disk inside the loop, so whichever disk was decided last would
silently overwrite an earlier disk's `ActionVMCapReached` — a VM where one disk capped and a
later-processed disk did not could export `vm_cap_reached=0`.

`vm_min_iops_floor_exceeds_pool` is `vm_cap_reached`'s static counterpart, not a duplicate of it: it
sums each managed disk's own resolved `min_iops` (configuration.md §5 — disk > vm > platform default)
across every disk sharing the VM and compares that sum against the VM's real pool (`VMIOPSPool`, §4.6),
independent of any observed load. `vm_cap_reached` says demand is currently outrunning the pool;
`vm_min_iops_floor_exceeds_pool` says the operator's own configured floors already cannot all be
honored at once, even with every disk sitting idle — a configuration problem to fix in the config file,
not a signal that the VM needs more vCPU. Set once per reconcile cycle the same way `vm_cap_reached` is,
over managed disks only (an unmanaged sibling's `min_iops` is not this agent's to account for).

`metric_source{disk,source}` sets **both** the guest and platform series on every call — the active
source's to 1, the other to 0 — rather than only ever setting the active one to 1. A `GaugeVec` only
remembers series someone has `Set()`; setting just the active source left the previously active
source's series stuck at 1 forever the moment a disk's source changed (relevant once Phase 2
comparison/source switching, US-005b, exists).

`disk_latency_baseline_seconds` has no `{op}` label, unlike `disk_observed_iops` — the baseline (§4.4) is
one combined low-percentile over both read and write latencies, not tracked separately per operation
(§5.2's `LatencyStat` is per-op for the *trigger* comparison, but the baseline itself is not); an
`{op}` label here would imply a distinction the value doesn't actually carry.

M5 wires every metric above except `writes_total`, `write_duration_seconds` (no Actuator exists yet to
report a write outcome — M6), and `cost_delta_iops_hours` (needs a billing-rate model not designed
anywhere yet) — registering a metric with nothing real to report would repeat the exact
`*_quota_utilization`-reads-zero trap docs/platform-reference.md documents as a platform bug, not something to imitate.
Those three are added when their producing subsystem exists.

`metric_source` and `staleness` are there because the metric contract supports two sources (§4.2): when `local` and
`fleet` disagree, the first question is which source fed the decision and how stale it was.

`cost_delta_iops_hours` accumulates billable IOPS-hours above the free 1000 base, so the value the
autoscaler delivers (or costs) is measurable rather than asserted.

`disk_configured_unmanaged{device_name,reason}` is the one gauge here whose label set is unbounded and
operator-controlled rather than fixed in advance — `device_name` is whatever a `disks.selected`/
`disks.exclude` entry named, which may not correspond to any live disk at all (§4.1). It is therefore
the one metric in this file that is *deleted*, not set to 0, once resolved
(`telemetry.SyncDiskConfiguredUnmanaged`): a `GaugeVec` series set to 0 would still enumerate in
`/metrics` forever, permanently listing every name an operator ever mistyped.

### 7.2 Decision journal

Every reconcile emits one structured record per disk, including no-action:

```json
{
  "ts": "2026-08-28T15:04:05Z",
  "disk": "db-data", "vm": "database-01", "class": "dependent",
  "action": "no_action",
  "reason": "saturation_gate_not_met",
  "current_iops": 5000, "target_iops": 5000, "demand_iops": 0,
  "utilization": 0.62,
  "queue_depth": {"value": 0, "unit": "ops"},
  "p95_read_latency_ms": 1.2, "p95_write_latency_ms": 0.9, "latency_baseline_ms": 1.1,
  "vm_headroom_iops": 4200, "budget_remaining": 47,
  "window_fill_up": 1.0, "window_fill_down": 1.0,
  "iops_demand_down": 0, "bandwidth_demand_down_iops_equiv": 0, "limiting_resource_down": "",
  "source": "guest"
}
```

`action`/`reason` are snake_case throughout, matching `Reason`'s own "machine-parseable" requirement —
one casing convention across the whole record, not `Action` in one case and `Reason` in another.
`target_iops`/`demand_iops` equal `current_iops`/`0` on a `no_action` record like this one, which is
why the original draft of this example omitted them; a `scale_up`/`scale_down` record needs both
visible. `window_fill_up`/`window_fill_down` are separate because they're independent windows
(§4.2) — a disk can be full on one and not the other.

`iops_demand_down`/`bandwidth_demand_down_iops_equiv`/`limiting_resource_down` (issue #17) are the
down-window demand breakdown behind `demand_iops` on a `scale_down` record — `demand_iops` there
equals `EffectiveDemandP95Down`, whichever of the other two is larger; `limiting_resource_down` names
which one. They read `0`/`""` here, as in this example, whenever the record isn't an underutilized
verdict at all (the down-window figures are still computed and journaled on every cycle, per §4.5, but
only a `scale_down` or `Underutilized`-but-suppressed record makes them meaningful).

This is the primary calibration input for the `--dry-run` harness (§8.5) — thresholds are tuned from
these records, not from reasoning.

**`no_data` / `metrics_stale` / `discovery_stale`** (pre-merge review) are three operationally distinct
reasons an operator must be able to tell apart from the journal alone — "this disk has never produced a
sample" is not the same fact as "this disk's data went stale" or "the discovery snapshot itself can no
longer be trusted," and each implies a different fix. `no_data` and `metrics_stale` are two of Decide's
own default-branch outcomes (§4.3, and its `Decide` pseudocode's comment above); `discovery_stale` is
different in kind — it never reaches `Decide` at all, emitted instead by the orchestrator when a whole
cycle is skipped (`!managedValid` right after a fatal `discover()` error, or the snapshot older than
`max_discovery_age`, §7.3/§4.7). A `discovery_stale` record's `Inputs`/`vm_headroom_iops`/
`budget_remaining` are honestly zero-valued rather than fabricated: this cycle never called `Decide`, so
there is no real `Analysis` or guardrail state behind it. Every disk still sitting in the untrusted
managed set gets its own `discovery_stale` record per skipped cycle — a cycle skipped for stale
discovery must remain answerable from the journal alone, the same guarantee every other cycle already
gets.

### 7.3 Health and alerting

`/healthz` (process alive) and `/readyz` (config valid, discovery succeeded, ≥1 source reachable, budget
healthy).
`source_reachable` is driven by the Sampler's own `Tick()` result, not by Compute API discovery
succeeding (issue #15) — even in `local` mode these are different dependencies: the guest source reads
`/proc/diskstats`, which can fail (permissions, a missing disk) independent of whether the Compute API
is reachable. Conflating the two previously meant a metrics-collection outage never showed up in
`/readyz` as long as discovery kept succeeding. A successful `Tick()` alone is not sufficient, though
(issue #4): it also requires the managed set to be non-empty, since a `Tick()` over zero disks trivially
"succeeds" without having verified anything is actually reachable.

`budget_healthy` (issue #10) is `false` once any durable budget persistence operation
(reserve/confirm/release) has failed during this process's run — polled once per reconcile cycle from
`state.Tracker.Healthy()`. Recovery requires a restart (which re-runs `LoadBudget` and the startup
reconciliation pass below), not a wait, matching how the other three conditions behave. Alert-worthy on
its own: an operator seeing `/readyz` fail for this reason specifically knows the fix is "restart," not
"wait for the network to come back."

`/readyz`'s JSON body also carries `configured_disks_unmanaged` — every currently-degraded
`disks.selected`/`disks.exclude` name (§4.1), as `{device_name, reason}` — but this field is reported
*alongside* the binary verdict above, never folded into it: a degraded disk at runtime does not make
`/readyz` unready. The field is omitted entirely when nothing is degraded, so the healthy-state
response is byte-for-byte what it was before this field existed.

Alert-worthy: metric staleness beyond 3 publication intervals; write failure rate above threshold;
change budget exhausted; `VM_IOPS_CAP_REACHED` sustained (actionable — add vCPU); any disk pinned at
`max_iops` while still saturated (the ceiling is too low); any entry in `configured_disks_unmanaged`
sustained beyond a few discovery cycles (a typo an operator should fix, not merely observe).

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
- `NoAction` whenever the window is not full
- `NoAction` only for the specific evaluation cycle where every applicable signal needed a baseline
  that isn't established yet — never permanently, and never when queue depth or throttling alone
  already confirm saturation (§4.4)
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
| No scale-up when efficient | `fio --rate_iops` just under limit, shallow queue | **No change** — the saturation-only invariant (§4.4) |
| Scale-down after quiet | stop load, wait `scale_down_window` | IOPS lowered, never below `min_iops` |
| No flapping | alternating bursts around the threshold | Changes ≤ expected given hysteresis and cooldown |
| VM cap detection | load both disks simultaneously past 6000 aggregate | `VM_IOPS_CAP_REACHED`, no write |
| Bandwidth-bound case | `fio bs=64k` — pins guest `%util` at 100 and `bandwidth_utilization` at the ceiling while `iops_utilization` reads ~51% | Gate opens on bandwidth (§4.4); saturation detected via latency/queue, **not** missed |
| Dependent write path | drive the VM-owned disk | Change applied via VM path; all other fields unchanged |
| Independent write path | drive the ref-attached disk | Change applied via disk path |
| Cold start | restart mid-load | No action until window full |
| Dry run | `--dry-run` under load, both classes | Independent: `ValidateOnly` used, server verdict. Dependent: computed verdict only (§4.7). Zero mutations either way |

The bandwidth-bound scenario is the one that catches a whole class of bugs: a naive IOPS-only
controller sees 513/1000 and concludes there is headroom, while the disk is at 100% utilization with
265 ms write latency and a queue of 135.

### 8.4 Load and soak

- **Fleet scale:** 100 synthetic disks against the fake API — assert NFR-2 (one metrics call per
  project per cycle) and NFR-3 (memory).
- **Soak:** 24 h against the live environment with a diurnal load pattern; assert change count is
  sane, no unbounded memory growth, no budget exhaustion under normal load.
- **Chaos:** random injected API failures at 20% — assert no double-applies and no state divergence.

### 8.5 Calibration harness (`--dry-run`)

Not a test but the mechanism that makes the defaults defensible: run `--dry-run` (US-013) against
real workloads, record every decision that *would* have been made plus what the load did next, then
tune thresholds against that corpus. This is the same validation step §9.2 folds into M6 before
actuation is flipped on — a mechanism used once as a gate before GA, and again wherever recalibration
is needed — not a separate observe-only release phase of its own (§9.2). **No default in
`docs/configuration.md` should be treated as validated until it has survived this.**

---

## 9. Implementation plan

### 9.0 CLI flags

`cmd/mws-iops-autoscaler` (the orchestrator wiring every package above together — §3.1's whole
pipeline, minus systemd/`.deb` packaging and live validation, both deliberately left for a human step,
not this binary's own job):

| Flag | Meaning |
|---|---|
| `-config <path>` | Required. |
| `-validate-config` | Parse and validate, then exit (§9.1's M0). |
| `-dry-run` | US-013 — discover, sample, decide; apply no change. |
| `-state-dir <path>` | Where the durable 24h change budget lives (§9.4). Default `/var/lib/mws-iops-autoscaler`. |
| `-journal <path>` | Decision-journal (§7.2) output file. Default stdout. |
| `-listen <addr>` | `/metrics`, `/healthz`, `/readyz` (§7.1/§7.3). Default `:9435`. |

Project, zone and VM name are resolved from the instance metadata service before `mws.Load`
(`compute.ResolveSelfIdentity`) — not flags, since they are credential-adjacent values
tied to the VM the process is actually running on, not a run-mode choice. `MWS_PROJECT`/`MWS_ZONE`
(the SDK's own env vars, `go.mws.cloud/go-sdk`'s table above) are not read by this binary at all:
local mode always runs inside the target VM, so there is no scenario in which pointing it at a
different project or zone than the one it is actually running in would be correct, and no override
path exists to do so. `runtime.metrics.source: platform` in the config is rejected with a clear "not
implemented yet" rather than silently falling back to guest — `PlatformSource` is M7.

### 9.1 Milestones

Product-facing phasing — what ships, in what order, and why — lives in the PRD, §6 "Release Phases".
This table is the engineering sequencing underneath it; the **Phase** column is the mapping. Ordering
rationale (local complete before fleet exists at all; platform metrics validated in `local` before
`fleet` depends on them exclusively) is §9.2.

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
all** (M0–M9 precede M10). See §9.2 for the full comparison against the alternative orderings.
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

- **Cooldown persistence (only).** v1 keeps cooldowns in memory, so a restart forgets them. This is
  safe, not just cheap: `cooldown.scale_up ≤ scale_up_window` and `cooldown.scale_down ≤
  scale_down_window` are both enforced by `--validate-config`, so the cold-start rule (no action
  until windows refill) always takes at least as long as the cooldown it would have replaced. Revisit
  if restarts prove frequent. **The 24h change budget is not part of this deferral** — see §9.4:
  the cold-start bound says nothing about total changes across many restarts in a day, so the budget
  counter must be reconstructible from durable storage on startup. M4 (Decider + Guardrails) is not
  done without it. Nor is a restart-only guarantee enough on its own (issue #10): a live-actuation
  process can also crash *between* a write landing and the budget record for it becoming durable, or
  suffer a mid-run persistence failure with no restart involved at all — see the crash-safety note
  right below, §9.4's durability rule, and `internal/state`'s `Reserve`/`Confirm`/`Release` intent/result
  log.
- **Budget crash-safety (issue #10).** The durable budget is a small intent/result state machine, not
  a single "record after success" line: `Tracker.ReserveChange` durably reserves a disk's budget slot
  *before* the write is attempted (`state.StatusPending`), so a crash between the platform confirming a
  write and this process recording that fact can never silently undercount a real change — the
  reservation already exists and already counts. `ConfirmChange`/`ReleaseChange` resolve it afterward.
  A reservation a previous run never resolved survives a restart as `Pending` and is reconciled once,
  at startup right after the first successful discovery, against a fresh live read of the disk: live
  current equal to the reservation's target confirms it; equal to its pre-write value releases it;
  anything else is left pending rather than guessed at. Any persistence failure — at reservation,
  confirmation, or release — marks `state.Tracker` permanently unhealthy for the rest of the process's
  life; `Tracker.State` then reports `ChangesInLast24h` as an effectively-unbounded sentinel, so
  `Decide`'s existing `budget_exhausted` path refuses every further write without the Decider needing a
  second signal to know about. `Agent.actuate` also checks `Tracker.Healthy()` directly, to avoid even
  attempting a reservation that would just fail again. `LoadBudget` itself fails closed on corruption: a
  bad line anywhere but the very last one (which can only be an in-progress append torn by a crash,
  recoverable by dropping it) refuses to start rather than silently skip the line and understate the
  count.

  **The same three-way resolution also applies mid-run, immediately after a failed write, not only at
  startup.** A non-dry-run `actuator.Apply` error does not prove the change never landed — the platform
  may have applied it before the response reached this process. Releasing on the error alone, as an
  earlier version of this logic did, violates `Budget.Release`'s own contract (live state verified to
  still match the reservation's `Before`) and can silently undercount a change that actually happened.
  `Agent.resolveReservationAfterWriteFailure` re-reads live state the same way startup reconciliation
  does and resolves from that: live equal to `Target` confirms the reservation *and* adopts the value
  into `a.managed`/`disk_provisioned_iops` exactly as a verified success would (issue #16/#19 — the next
  cycle must decide from reality, not a stale `Current`); live equal to `Before` releases it; anything
  else, including a failed read, leaves it pending until the next restart's reconciliation. A dry run is
  exempt from this — it never reaches a mutating call that could ambiguously succeed (an independent
  disk's dry run goes through the platform's own `validateOnly`, contractually non-mutating even on
  error; a dependent disk's dry run returns before `UpdateDependentDisk` is ever called, §4.7) — so a
  dry-run error still releases unconditionally.
- **Leader election.** Two `fleet` agents on the same project would both act. v1 documents
  "run one"; a lease is the fix if it becomes real.
- **Absolute latency thresholds.** Relative-to-baseline only in v1; `latency.mode: absolute` is in the
  config schema's type but rejected by validation (issue #6) — the field decodes, but only "relative"
  passes `--validate-config`, since no Analyzer code path reads the mode at all yet.

---

## 10. Open questions

1. **Resize rate limits.** Undocumented. Determines whether `max_changes_per_24h` (48) is
   conservative or already too high. Test empirically before M8.
2. **`*_quota_utilization` fix.** Committed by the NBS team. When it lands, it becomes the cheap
   utilization gate and removes a `GetDisk` per disk. Note the trap: today `0` means "broken"; after
   the fix `0` legitimately means "idle" (§4.4).
3. **Operations-denominated queue depth.** Committed. Would let `fleet` use the same saturation logic
   as `local`, narrowing the local/fleet capability gap (§4.2).
4. **Read queue metric.** Absent. Until it exists, read-heavy saturation in `fleet` rests on latency
   alone.
5. **Demand estimator validity.** The core open question. Both regimes in §4.5 are plausible and
   unproven; M6 exists to settle them.
6. **Correlated bursty siblings.** p95 headroom may be too optimistic when several disks burst
   together. Needs multi-disk load data.
7. **Minimum economically meaningful scale-down.** Given the 100-IOPS step and per-minute metering,
   below some delta a change costs more in churn than it saves.

---

## 11. External algorithm references

- [AWS RDS storage autoscaling](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIOPS.Autoscaling.html):
  dwell time of at least five minutes, at most four modifications per 24 hours, a hard
  `MaxAllocatedStorage` ceiling, no scale-down, and a step based on the largest of 10 GiB, 10% of
  current size, or predicted seven-hour growth.
- [Azure Database for MySQL Flexible Server — autoscale IOPS](https://learn.microsoft.com/en-us/azure/mysql/flexible-server/concepts-storage-iops):
  uses Storage IO Percent as its signal and charges for consumed operations, unlike MWS provisioned
  IOPS billing.
