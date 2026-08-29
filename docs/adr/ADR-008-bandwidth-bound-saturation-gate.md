# ADR-008: The saturation gate opens on IOPS *or* bandwidth utilization, not IOPS alone

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Supersedes:** the IOPS-only gate formula in `docs/design.md` §4.4; amends ADR-002's gate, not its
"gate, not a trigger" principle

## Context

ADR-002 established the saturation gate: utilization must clear a threshold before queue depth,
latency, or throttling are even consulted — "a disk below it cannot be limit-starved, so the expensive
checks are skipped." It was written IOPS-only, and the field name says so:

```
saturated :=
      utilization >= policy.saturation.iops_utilization_threshold   (gate, default 0.98)
  AND ( queue_depth >= ... OR p95_latency > ... OR throttling_detected )
```

This repo's own E2E test plan (design.md §8.4) requires a bandwidth-bound scenario to pass: `fio
bs=64k` pins the disk at its throughput ceiling while IOPS utilization sits around 51% (roughly
513/1000 ops — large blocks mean fewer, bigger operations). NBS-PL2's IOPS and bandwidth are linked by
a fixed ratio (CLAUDE.md, "Disk NBS-PL2": `MB/s = IOPS × 32 / 1024`), so a large-block workload
saturates bandwidth long before it saturates the IOPS count. Under the gate as written, that scenario
can never reach a decision: IOPS utilization (0.51) never clears 0.98, so queue and latency — even
though they clearly show unmet demand — are never even checked. The algorithm and the test the repo
already commits to run against it disagree with each other.

This is not a missing feature. NBS-PL2 disks are provisioned and billed on IOPS, but large-block,
throughput-heavy workloads are a normal and expected use of them — the gate silently failing exactly
that case defeats the product's purpose for a whole class of real workloads.

## Decision

**The gate opens when either IOPS utilization or bandwidth utilization clears the threshold** — not
only IOPS. Confirmation (queue, latency, or throttling) is still required exactly as before; ADR-002's
core principle — utilization alone never triggers a change — is unchanged, the gate just now looks at
two provisioned resources instead of one:

```
resource_gate :=
      iops_utilization      >= policy.saturation.utilization_threshold
   OR bandwidth_utilization >= policy.saturation.utilization_threshold

saturated :=
      resource_gate
  AND ( queue_depth >= ... OR p95_latency > ... OR throttling_detected )
  AND sustained over policy.evaluation.scale_up_window
```

`bandwidth_utilization = (read_bytes/s + write_bytes/s) / (spec.iops × 32768)` — the denominator is
the disk's own effective throughput ceiling, using the exact per-IOPS byte figure CLAUDE.md measured
(`33554432 Bps` at 1000 IOPS = 32768 bytes/IOPS), not an approximation.

**The config field is renamed** `saturation.iops_utilization_threshold` →
`saturation.utilization_threshold` — it now gates on whichever provisioned resource is closer to its
ceiling, so a name that says "IOPS" specifically would be wrong. Default unchanged at `0.98`. No Go
code exists yet to rename (the Analyzer this feeds is M3, not built) — this is a documentation-only
change today, corrected before it becomes a shipped field name.

## Options Considered

### Option A: A separate `bandwidth_utilization_threshold` knob

Lets the two ceilings be tuned independently. Rejected for now: nothing in the product distinguishes
"how close to IOPS-full is too close" from "how close to bandwidth-full is too close" in a way an
operator would want to set differently on day one — both express the same "provisioned and paid for,
so this alone isn't a problem" idea from ADR-002's own "why utilization is only a gate" reasoning.
Adding a second knob later is a strictly additive change if calibration data ever shows a real reason
to split them (design.md §8.5).

### Option B: Chosen — one shared threshold, gate opens on whichever resource is higher

### Option C: Leave the gate IOPS-only; treat the bandwidth-bound E2E scenario as aspirational and drop it

Rejected. Large-block, throughput-heavy workloads are a normal, common case for a disk billed and
provisioned the way NBS-PL2 is — dropping the scenario doesn't remove the problem, it just stops
testing for it. This is a real gap in what the product does, not test-authoring debt.

## Consequences

**Easier**
- The bandwidth-bound E2E scenario (design.md §8.4) becomes reachable — the algorithm and the test
  plan agree.
- One config knob, not two.

**Harder**
- The Analyzer needs the disk's `spec.iops` to derive the bandwidth ceiling. Already available: M1's
  `Discover` already reads `spec.iops` per disk (`compute.Disk.SpecIOPS`) on the same discovery cycle
  — no new API call, just a formula the Analyzer applies alongside the IOPS-utilization one.

**Revisit when**
- Calibration telemetry (design.md §8.5) shows the shared threshold behaves poorly for one dimension
  specifically — split into Option A's two knobs then, with evidence instead of speculation.
