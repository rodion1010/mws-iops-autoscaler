# ADR-012: Baseline establishment is never blocked by saturation; only its updates are frozen

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Amends:** `docs/design.md` §4.3's `baseline_not_established` gate, §4.4's baseline description,
§4.5's demand estimation, §8.1's cold-start test invariant, and NFR-9 — no shipped code changes (the
Analyzer this affects is M3, not yet built)

## Context

§4.3's reconcile loop gates every decision on two cold-start checks in sequence:

```
if window_not_full(disk):           skip   ← cold start never acts
if baseline_not_established(disk):  skip   ← latency baseline warm-up
```

§4.4 describes the baseline itself: "a long rolling low-percentile of the disk's own observed
latency, **frozen while the disk is saturated** so a throttled period cannot poison it." Read
together, these two statements describe a real deadlock: the baseline can only be learned from
samples taken while *not* saturated (frozen means never updated during saturation), but no decision —
including the one that would relieve the saturation — can be made until the baseline exists. An agent
that starts against an already-saturated disk (a very ordinary case: installing the autoscaler onto
an existing, already-struggling workload is a plausible first day) never collects a sample the
"frozen while saturated" rule will let it learn from, never establishes a baseline, and therefore
never acts — permanently, not just at startup. §8.1's own property-based test invariant states the
blanket form of the bug directly: "`NoAction` whenever the window is not full or the baseline is
unset," with no exception for a saturation signal that never needed the baseline in the first place.

The trigger condition in §4.4 is already an OR of three independent signals:

```
saturated :=
      resource_gate
  AND ( queue_depth  >= policy.saturation.queue_depth_threshold
     OR latency.P95  >  baseline.P95 × policy.saturation.latency.baseline_multiplier
     OR throttling_detected )
  AND sustained over policy.evaluation.scale_up_window
```

Only the middle disjunct needs a baseline. `queue_depth` is a direct threshold comparison against
`Sample.QueueDepth`, needing nothing but the window. `throttling_detected` (§4.5) is
`served_iops ≈ provisioned_limit AND queue_depth sustained > 0` — again nothing but the window. The
blanket `baseline_not_established(disk): skip` gate blocks both of these unconditionally, even though
neither one was ever the thing waiting on a baseline.

## Decision

**Two independent fixes, both required — either alone still leaves a gap:**

**1. Baseline establishment is not gated by saturation.** "Frozen while saturated" describes what
happens to an *already-established* baseline — it must not be revised downward-in-quality by
throttled samples once it exists. It never described *establishing* one. The baseline seeds from the
disk's first full window's low-percentile latency, computed exactly the same way whether or not the
disk happens to be saturated during that window — a low percentile (not a mean, not the p95 the
saturation check itself uses) is already the statistic §4.4 chose specifically because it stays close
to true unloaded service time even under moderate load, not every request queues even on a busy disk.
Once established, it is frozen for the duration of an active saturation episode, exactly as before.
This closes the deadlock in the common case directly: a disk that is saturated from the very first
sample gets a baseline from that very first full window, not never.

**2. The blanket `baseline_not_established(disk): skip` gate is removed.** In its place, baseline
availability affects only the specific signal and demand-estimation form that actually need it:

```
if window_not_full(disk):  skip   ← cold start never acts; unaffected by this ADR

saturated :=
      resource_gate
  AND ( queue_depth       >= policy.saturation.queue_depth_threshold
     OR (baseline_established AND latency.P95 > baseline.P95 × policy.saturation.latency.baseline_multiplier)
     OR throttling_detected )
  AND sustained over policy.evaluation.scale_up_window
```

The latency disjunct simply evaluates false — never blocks, never falsely triggers — when no
baseline exists yet; `queue_depth` and `throttling_detected` are unaffected and can confirm
saturation on their own. §4.5's demand estimate has the same shape: the open-workload form
(`demand ≈ served + dL/dt`, queue growing) needs no baseline; only the closed-workload form
(`demand ≈ served × (latency.P95 / baseline.P95)`, queue stable and deep) does. In the one remaining
corner — saturation confirmed via queue/throttling, workload is closed, and (only possible on the
very first reconcile cycle after fix 1 above) the baseline has not yet been computed from this same
window — `demand()` has no input to compute a ratio from. That cycle emits `NoAction` with a specific
reason (baseline pending, evidence for saturation already present) rather than guessing; because fix 1
establishes the baseline from this same window regardless of saturation, the very next cycle has one
available. This is a single-cycle `NoAction`, matching NFR-9's "must not act on partial evidence"
intent, not the permanent deadlock the blanket gate produced.

`Analysis` (ADR-011) gets one more field: `BaselineEstablished bool`, distinct from `LatencyStat`'s
`Kind` — establishment is a state question ("do we have one yet"), `Kind` is a derivation-method
question ("how was this percentile computed"), and conflating them would make one field answer two
different things.

## Options Considered

### Option A: Leave the blanket gate, document the deadlock as a known limitation

Rejected. This is not a rare edge case worth documenting around — "install the agent on an
already-loaded disk" is an entirely ordinary first-day scenario, and the failure mode is silent and
permanent (no error, no retry that helps, just `NoAction` forever with no path out short of a config
change to disable the baseline signal or a manual resize to relieve pressure so a quiet sample can
finally land). A safety mechanism that can permanently disable the product for its most natural
first-use case is not a limitation to document, it is a bug to fix.

### Option B: Seed the baseline from an operator-supplied default instead of learning it

Rejected. §4.4's whole reason for a relative baseline over an absolute threshold is that "acceptable
latency is workload-specific" (ADR-002) — a supplied default reintroduces the exact problem the
relative design exists to avoid, and does so exactly in the case (already-loaded, unknown workload)
where a wrong guess is most costly.

### Option C: Establishment unblocked by saturation + signal-specific gating (chosen)

Both halves are needed together. Establishment-unblocked alone (without signal-specific gating) still
leaves one full cycle of the old blanket behavior at true cold start before the first window is even
full — acceptable, but only because signal-specific gating means that cycle produces a *precise*
`NoAction`, not a silent one indistinguishable from "everything is actually fine." Signal-specific
gating alone (without unblocking establishment) still permanently loses the latency signal and the
closed-workload demand form on any disk that stays saturated forever, since the baseline genuinely
never gets a chance to exist — a real degradation, just no longer a total deadlock.

## Consequences

**Easier**
- The agent can act on a disk that is saturated from the moment it starts managing it — the ordinary
  first-day case — instead of silently never acting.
- `NoAction` now distinguishes "baseline pending, one cycle of patience" from "genuinely no evidence
  of a problem," which §4.3's own requirement ("why didn't it scale? must always be answerable from
  the log alone") already demanded and the blanket gate could not honestly provide.

**Harder**
- The Analyzer needs to evaluate three independent conditions instead of one blanket precondition —
  more branches, but each one is simpler than the combined gate it replaces.
- `Analysis` carries one more field (`BaselineEstablished bool`).

**Revisit when**
- M3 (Analyzer) is implemented — this ADR's formulas become the actual `saturated()`/`demand()`
  implementation and its test suite, replacing §8.1's blanket cold-start invariant.

## Action Items

1. [ ] When the Analyzer (M3) is implemented: baseline seeds from the first full window's low
       percentile regardless of saturation state; freeze applies to updates only.
2. [ ] Remove the blanket `baseline_not_established(disk): skip` gate; make the latency disjunct in
       `saturated()` and the closed-workload form in `demand()` each individually conditional on
       `BaselineEstablished`.
3. [ ] Add a property-based test: a disk saturated on every sample from the first window onward still
       reaches a scale-up decision within `scale_up_window + one reconcile cycle`, replacing §8.1's
       "NoAction whenever ... the baseline is unset" invariant with the corrected, signal-specific
       version.
4. [x] `docs/design.md` §4.3/§4.4/§4.5/§5.2/§8.1 and NFR-9 corrected to reflect this.
