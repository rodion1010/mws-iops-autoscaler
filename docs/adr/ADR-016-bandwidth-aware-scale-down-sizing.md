# ADR-016: Scale-down sizing uses the aligned per-sample max of IOPS and bandwidth demand

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Amends:** design.md §4.3/§4.5 — extends ADR-008's "gate on whichever resource is closer to its
ceiling" reasoning from the saturation gate to scale-down target sizing

## Context

ADR-008 fixed the saturation gate so a large-block, low-ops/s workload could still trigger scale-up:
NBS-PL2 ties provisioned IOPS and bandwidth at one fixed ratio (`bytesPerIOPS = 32768`, CLAUDE.md), so
`bandwidth_utilization` had to become a second way to clear the gate alongside `iops_utilization`.

Scale-down sizing (design.md §4.3, US-010) had the identical, un-fixed bug: `target =
guardrails(m.IOPSP95Down / policy.target.scale_down_utilization, ...)` sized the new limit from served
ops/s alone. A workload doing 900 large-block (64 KiB) ops/s is only using 900 of a much larger IOPS
budget by count, but those 900 ops/s move ~59 MB/s — an IOPS-equivalent of `900 × 64KiB / 32768 ≈
1800`. Sizing the target from `IOPSP95Down = 900` alone would land the new limit at roughly half what
the workload's actual bandwidth needs on the very next cycle, reproducing ADR-008's original problem
one layer downstream: the gate now opens correctly, but the number the scale-down path picks once it
does doesn't account for the resource that's actually binding.

A second, more subtle problem exists even once bandwidth is added: computing `IOPSP95Down` and a
`BandwidthP95Down` independently and taking `max(p95(ops), p95(bandwidth))` is not the same as taking
the p95 of the per-sample max. Two percentiles computed independently over the same window can each be
driven by a *different* sample — an ops outlier at minute 12 and a bandwidth outlier at minute 20,
neither of which is extreme enough to surface in the other series' own p95. `max(p95_ops, p95_bw)`
misses both; only aligning the max per sample before taking the percentile catches whichever resource
was actually binding at each moment.

## Decision

**Scale-down sizing uses `EffectiveDemandP95Down`: the p95, over `scale_down_window`, of each sample's
own `max(ops, bandwidth ÷ bytesPerIOPS)`.**

```
per sample:  ops_i = read_ops_i + write_ops_i
             bw_iops_equiv_i = (read_bytes_i + write_bytes_i) / bytesPerIOPS
             effective_i = max(ops_i, bw_iops_equiv_i)

IOPSP95Down               = p95({ops_i})
BandwidthP95DownIOPSEquiv = p95({bw_iops_equiv_i})
EffectiveDemandP95Down    = p95({effective_i})            <- what §4.3 actually sizes from
LimitingResourceDown      = "bandwidth" if BandwidthP95DownIOPSEquiv > IOPSP95Down else "iops"
```

```
target = guardrails(EffectiveDemandP95Down / policy.target.scale_down_utilization, disk, vm, down)
```

`IOPSP95Down` and `BandwidthP95DownIOPSEquiv` are still computed and carried on `Analysis`, and
journaled alongside `LimitingResourceDown`, purely so a scale-down (or suppressed-Underutilized)
journal record shows *which* resource actually drove the number — the same "why didn't/did it scale"
requirement design.md §4.3 already holds every other branch to.

This mirrors ADR-008's shape exactly: one shared concept (NBS-PL2's fixed IOPS-to-bandwidth ratio)
applied at two different points in the pipeline — the gate (ADR-008) and the sizing (this ADR) — with
the same underlying justification and no new config surface.

## Options Considered

### Option A: `max(p95(ops), p95(bandwidth_iops_equiv))` — two independent percentiles, combined after

Simpler to implement (two separate percentile calls, no new per-sample loop) and was the first draft.
Rejected: it misclassifies a specific, real scenario — an ops spike and a bandwidth spike at different
moments, each below the other series' own p95 — as ordinary quiet demand, undersizing the scale-down
target exactly in the case where getting it right matters most (a workload that alternates between
op-heavy and block-heavy bursts). `TestDemandP95Down_alignedPerSampleMaxNotCombinedPercentiles` in
`internal/analyzer/analyzer_test.go` constructs this exact scenario and asserts the aligned-max value
(5000) is surfaced where the combined-percentiles value (100) would hide it.

### Option B: Chosen — per-sample `max(ops, bandwidth_iops_equiv)`, then one p95 over that series

### Option C: Leave scale-down sizing IOPS-only; treat bandwidth as scale-up's problem only

Rejected. The billing and provisioning model (CLAUDE.md, "Billing") is symmetric — a lowered IOPS
limit lowers the bandwidth ceiling by the same fixed ratio it raised it by — so a bandwidth-heavy
workload that scale-up correctly sized (ADR-008) can just as easily be undersized again on the very
next scale-down cycle if that path ignores bandwidth. Fixing one direction and not the other reproduces
the ADR-008 problem on a delay rather than solving it.

## Consequences

**Easier**
- Scale-down targets retain real headroom for both IOPS and bandwidth, not just IOPS count — a
  large-block workload's new limit is no longer systematically undersized on the throughput axis.
- The journal (`iops_demand_down`, `bandwidth_demand_down_iops_equiv`, `limiting_resource_down`) makes
  "why this target" answerable without recomputing anything, same standard as every other field there.

**Harder**
- One more per-sample pass in the Analyzer's down-window computation (`demandP95Down`), replacing the
  single `percentile95TotalOps` call it used to be. No new external dependency or API call — it reuses
  `bytesPerIOPS`, the same constant ADR-008 introduced.

**Revisit when**
- Calibration telemetry (design.md §8.5) shows `EffectiveDemandP95Down` behaves poorly relative to a
  simpler formula in practice — this, like ADR-008's shared threshold, is a reasoned default, not a
  measured-optimal one.
