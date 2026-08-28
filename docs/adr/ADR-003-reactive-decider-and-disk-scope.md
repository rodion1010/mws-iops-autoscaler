# ADR-003: Reactive sliding-window decider over independent disks only

**Status:** Accepted
**Date:** 2026-08-28
**Deciders:** Product Lead (NBS)

## Context

Two decisions that shape the core loop, kept together because they constrain each other: what
algorithm decides, and which disks it is allowed to decide about.

**On the algorithm.** The obvious reference is AWS RDS storage autoscaling, which predicts growth
seven hours ahead. That fits a domain with a wide dynamic range and an irreversible action. Ours has
neither: the range is 1000→10000, only **10×**, and every change is reversible in seconds.

Measured constraints:
- Detection latency ≈ 135 s (30 s publication + 60 s window + 62–90 s lag). No algorithm can react
  faster than its inputs.
- Metering is per-minute at hourly rates, so a short-lived change costs only its actual duration.
  Frequent adjustment is not inherently wasteful.
- Billing quantum is 100 IOPS, giving at most 90 distinct steps across the whole range.

**On disk scope.** A disk created inside a VM's spec is *owned* by that VM (`metadata.usages`
contains `own`) and **cannot be modified through the disk API** — the platform returns 403 naming the
VM. Only independent disks attached by `ref` are editable. This is ownership, not permissions: the
same credentials edit an independent disk attached to the same VM successfully.

## Decision

Use a reactive state machine over sliding windows with asymmetric up/down behaviour. Manage
independent disks only; classify dependent disks and report them as unmanageable rather than
attempting or silently skipping them.

## Options Considered

### Option A: Reactive sliding-window state machine (chosen)

| Dimension | Assessment |
|---|---|
| Complexity | Low — thresholds, windows, cooldowns |
| Cost | Negligible |
| Scalability | Excellent — O(1) state per disk |
| Team familiarity | High |

**Pros**
- Explainable. Every decision reduces to "percentile X over window Y crossed threshold Z", which an
  operator can verify from the same graphs.
- No training data, no model, no drift.
- Correctness is testable with synthetic load, which we already generate with `fio`.

**Cons**
- Purely reactive: the spike is already happening before it responds.
- Fixed thresholds may suit one workload shape and not another.

### Option B: Predictive, AWS-RDS style

| Dimension | Assessment |
|---|---|
| Complexity | High — forecasting plus a reactive fallback |
| Cost | Higher; needs history storage |
| Scalability | Medium |
| Team familiarity | Low |

**Pros**
- Could scale up before the spike lands.
- Handles predictable daily or weekly cycles well.

**Cons**
- 10× range means predictions resolve to one of a handful of steps. The precision has nowhere to go.
- Exact metric values are retained for 2 days, then aggregated — thin ground for forecasting.
- Failure mode is bad: a wrong prediction over-provisions silently and bills for it, with no
  saturation signal to correct it.

### Option C: Control-theoretic (PID)

| Dimension | Assessment |
|---|---|
| Complexity | Medium — but tuning is the hard part |
| Cost | Negligible |
| Scalability | Excellent |
| Team familiarity | Low |

**Pros**
- Smooth convergence; handles varying load without threshold cliffs.
- Well-understood theory.

**Cons**
- 135 s of dead time with a 100-IOPS quantised output is close to the worst case for a PID. Dead time
  drives oscillation, and quantisation defeats the integral term.
- Tuning gains per workload is harder to explain than a threshold, and gets no better behaviour here.

## Trade-off Analysis

**Algorithm.** The narrow range decides it. With ~90 possible steps and a 135 s feedback delay,
sophistication has nowhere useful to go: a predictor's output collapses onto the same few values a
threshold would pick, while adding a failure mode where over-provisioning is invisible. Option A is
chosen not as a simplification but because the problem genuinely does not need more.

The asymmetry matters more than the algorithm class. Scaling up late costs performance during a
spike; scaling down late costs a little money briefly. So up is fast and decisive (5 min dwell, 20%
steps), down is slow and cautious (20 min window, p95, 10% steps). The safety on the way down comes
from the **percentile**, not the window length — requiring p95 below 40% means even the busiest
moments were quiet, which is a strictly stronger claim than a low average and lets the window be
20 minutes instead of an hour.

**Disk scope.** Supporting dependent disks means a full read-modify-write through `vm update`,
resending `diskType`, `iops` and `size` together. That is a second code path with a genuine race: two
concurrent VM updates can clobber each other's fields, and the agent would be writing VM-level
objects to change a disk-level property. For an MVP whose whole safety story is "we only ever touch
`spec.iops`", that is a poor trade.

The cost is real and worth stating plainly: **a VM's boot disk is always dependent**, so it is always
out of scope. Any user expecting their boot disk autoscaled will be disappointed, and the agent must
say so explicitly rather than omitting it from the report.

## Consequences

**Easier**
- The decider is a pure function of a metric window and config — trivially unit-testable.
- Every write is a single-field update to one disk. No VM-level writes, no cross-field races.
- Behaviour is explainable from the same graphs the operator already has.

**Harder**
- Cannot anticipate a known daily spike; it reacts after the fact.
- Boot disks are unmanageable, which needs to be prominent in the docs, not a footnote.
- Users who created data disks inline in the VM spec (the natural thing to do) will find them
  unmanageable and must recreate them as independent disks to benefit.
- Fixed thresholds will not suit every workload; the config surface exists to compensate, which
  shifts burden to the operator.

**Revisit when**
- Real workloads show the reactive loop is too slow in a way that matters. Add a scheduled-boost
  override before adding prediction.
- Enough users hit the dependent-disk wall to justify the `vm update` path.
- The platform's committed metric fixes reduce detection latency enough to make finer control useful.

## Action Items

1. [ ] Implement the window evaluator with configurable percentiles per direction (US-007).
2. [ ] Implement `classify()` from `metadata.usages`; dependent disks are reported, not skipped
       (US-004a).
3. [ ] Make "boot disks cannot be autoscaled" a first-class line in the README, not a caveat.
4. [ ] Validate the 80/40 threshold pair against a real workload before treating it as settled.
5. [ ] Add an integration test that drives `fio` on the test VM and asserts the decision sequence.
