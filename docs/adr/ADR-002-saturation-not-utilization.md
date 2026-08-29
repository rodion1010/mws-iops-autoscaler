# ADR-002: Scale on saturation, not on utilization

**Status:** Accepted
**Date:** 2026-08-28
**Deciders:** Product Lead (NBS)
**Supersedes:** the utilization-threshold trigger in the PRD's first draft

## Context

The obvious design is a utilization target: keep the disk at ~80% of its provisioned IOPS, scale up
above that, down below. This is what most autoscalers do and what the first PRD draft specified.

**It is wrong for provisioned IOPS.** With consumption billing, high utilization means you are about
to pay more, so it is worth reacting to. With provisioned billing, you have *already paid* for the
limit. A disk sitting at 100% of a limit it is paying for is using the resource efficiently:

```
Provisioned: 5000 IOPS
Actual:      5000 IOPS
Latency:     stable
Queue depth: ~0
```

Nothing is wrong here. Raising the limit adds cost and buys nothing. A utilization-driven controller
would scale up anyway.

The question the controller must answer is not *"how close is actual to provisioned?"* but *"does the
workload want to do more I/O than it is currently allowed?"* Those differ precisely when it matters.

This is also why the observability ceiling is a real problem: at the limit the IOPS counter pins and
stops carrying information. Saturation signals — queue depth, latency against baseline, throttling —
are what distinguish "efficiently saturated" from "starved".

## Decision

Scale up only on **confirmed saturation**: utilization near the limit **as a precondition**, plus at
least one independent saturation signal, sustained over a window. Compute the target from observed
demand and a target utilization, not by stepping a percentage.

The objective is:

```
minimize(provisioned IOPS)
subject to:  acceptable latency
             acceptable queue depth
             no sustained throttling
             disk and VM limits
```

## Options Considered

### Option A: Utilization target (rejected — was the first draft)

| Dimension | Assessment |
|---|---|
| Complexity | Low |
| Cost impact | **Bad** — scales up on efficient workloads |
| Explainability | High |
| Correctness | Wrong objective for provisioned billing |

**Pros**
- Trivial to implement and explain.
- Standard, familiar shape.

**Cons**
- Systematically over-provisions any workload that efficiently uses what it pays for.
- Cannot distinguish "at the limit and fine" from "at the limit and starved" — the exact distinction
  that matters.
- Optimises a proxy metric rather than the thing the user cares about.

### Option B: Saturation-driven with computed target (chosen)

| Dimension | Assessment |
|---|---|
| Complexity | Medium — several signals to combine |
| Cost impact | Good — only pays when there is unmet demand |
| Explainability | Medium — needs the reasoning surfaced |
| Correctness | Matches the actual objective |

Scale-up condition:

```
utilization >= utilization_threshold        (precondition, default 0.98)
AND (
      queue_depth >= queue_depth_threshold
   OR p95_latency > baseline_p95 × baseline_multiplier
   OR throttling_detected
)
AND sustained over scale_up_window
AND vm_headroom allows a useful increase
```

Target:

```
target = observed_demand / target_utilization      (default 0.85 up, 0.70 down)
```

then rounded to the 100-IOPS billing step, clamped to `[min_iops, effective_max]`, and rate-limited.

**Pros**
- Only spends money when there is demand that cannot be served.
- Target sizing responds to how much demand there is, not a fixed percentage step.
- Scale-down leaves deliberate headroom (`/0.70`), so it does not land flush against current demand.

**Cons**
- More signals means more calibration surface.
- Needs a per-disk latency baseline, which needs a warm-up period.
- Harder to explain than "we keep you at 80%".

### Option C: Control-theoretic (PID)

Rejected. 135 s of dead time in `fleet` mode with a 100-IOPS quantised output is close to the worst
case for a PID: dead time drives oscillation and quantisation defeats the integral term. Per-workload
gain tuning is also harder to explain than a threshold, for no better behaviour.

## Trade-off Analysis

**On latency thresholds: relative, not absolute.** An absolute `latency > 10 ms` is wrong for
everyone — acceptable latency depends entirely on the workload. `p95 > baseline_p95 × 1.5` adapts to
each disk. The cost is needing a baseline before the agent can act, which fits the phased rollout
below: observe-only builds baselines anyway.

**On utilization's remaining role.** It stays as a cheap precondition at 0.98 — if a disk is not near
its limit, it cannot be limit-starved, so the expensive signal checks can be skipped. It is a gate,
never a trigger.

**On the VM pool.** Headroom is dynamic, not a static split:

```
vm_headroom       = vm_iops_limit − p95_aggregate_iops_of_other_disks
effective_ceiling = min(disk_max, policy_max, vm_headroom)
```

Sibling disks are bursty, so this uses a rolling p95 of their actual consumption rather than their
provisioned limits. When a disk is saturated *and* the VM pool is also saturated, the controller must
not perform a cosmetic resize. It emits `VM_IOPS_CAP_REACHED` and reports that the bottleneck is
vCPU. This matters more here than on most platforms because **the VM ceiling is not enforced** — the
API happily accepts 10000 IOPS on a VM that can deliver 6000, and bills for it.

**On exposure.** The knobs above are not a user interface. Users pick a preset — `balanced` (default),
`performance`, `cost` — and the underlying values can be recalibrated without breaking anyone.
Advanced overrides exist but are opt-in.

## Consequences

**Easier**
- Efficiently-saturated workloads are left alone, which is the single biggest cost win.
- The objective is stated explicitly, so decisions can be argued about on their merits.
- Presets keep the user-facing surface small while the internals stay tunable.

**Harder**
- Three signals plus a baseline is more to calibrate than one threshold. The defaults are *design
  defaults*, not validated values.
- A cold agent has no baseline and must not act until it has one.
- "Why didn't it scale up?" becomes a real support question — the answer ("you were at the limit but
  not starved") is correct but counter-intuitive. The dry-run report must make it legible.
- Throttling is listed as a signal but **no throttling metric exists today**. The condition is
  written so it degrades gracefully to queue-plus-latency.

**Revisit when**
- Observe-only data arrives. Every threshold here is a guess until then.
- `*_quota_utilization` is fixed — it becomes the cheap precondition and removes a `GetDisk` call.
- A real throttling counter appears — it is the most direct saturation signal possible and should be
  promoted above the inferential ones.

## Rollout

Thresholds cannot be validated by reasoning, so the rollout is staged:

1. **Observe-only** — decide and record, change nothing. Captures detected saturation, recommended
   target, expected cost delta, and what the load actually did next. This calibrates the defaults.
2. **Scale-up only** — automate the direction where being wrong costs money but not performance.
3. **Bidirectional** — add conservative scale-down.
4. **Predictive** — only once there is history worth predicting from.

> **Amended by ADR-005.** Steps 1 and 2 above are no longer separate *releases* — scale-up actuation
> ships in the MVP itself, with observe-only (`--dry-run`) kept as the validation an operator runs
> before flipping to live, not a release gate. Steps 3 and 4 are unaffected: bidirectional still
> follows scale-up-only, and predictive is still out of scope for v1 entirely. The calibration
> purpose of observe-only (this section's whole point) is unchanged — only its packaging as a
> separate numbered release is revised.

## Action Items

1. [ ] Implement saturation detection with utilization as a gate, not a trigger.
2. [ ] Implement per-disk latency baselines with an explicit warm-up before the agent may act.
3. [ ] Implement `target = demand / target_utilization`, then step-round, clamp, rate-limit.
4. [ ] Implement dynamic `vm_headroom` from sibling p95, not provisioned limits.
5. [ ] Implement the `VM_IOPS_CAP_REACHED` state as a first-class outcome, not an error.
6. [ ] Ship observe-only first; treat every default as unvalidated until it has run.
7. [ ] Define `balanced` / `performance` / `cost` presets as the only default-facing surface.
