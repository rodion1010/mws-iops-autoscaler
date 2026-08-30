# ADR-017: Queue-depth unit is checked, not assumed, before it enters IOPS demand math

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Amends:** ADR-001's own "Harder" consequence — "Mixing them silently would be a real bug — units
must be explicit at the interface boundary" — by making that boundary a runtime check, not just a
documented intent

## Context

ADR-001 already named the risk: guest's `QueueDepth` is operations-denominated, platform's is
byte-denominated (`compute_vm_disk_write_queue_length`, CLAUDE.md), and `metrics.QueueDepth` carries an
explicit `Unit` field so "mixing them silently would be a real bug" never happens undetected. That
field existed; nothing read it.

`decider.demand()`'s open-workload estimator computed `served + dL/dt` — `ServedIOPS` (ops/s) plus
`Analysis.QueueGrowthRate`, taken at face value regardless of what unit produced it. The Analyzer's own
queue-threshold predicates (`sustainedQueueAtOrAbove`/`sustainedQueueBelow`, feeding `Saturated` and
`Underutilized`) had the identical gap: they compared `QueueDepth.Value` against
`policy.saturation.queue_depth_threshold` with no check that the value was denominated the way the
threshold assumes.

This is unreachable through the pipeline shipped today — `GuestSource` is the only `MetricSource`
implemented, and it only ever produces `QueueDepthOps`. It is exactly the trap ADR-005 defers `fleet`
mode to avoid building around blind: the moment M7's `PlatformSource` exists and starts returning
`QueueDepthBytes`, the same code paths would silently add a bytes/s reading into an ops/s sum, or
compare it against an ops-calibrated threshold — producing, per the bug report, "typically a huge
nonsensical demand that guardrails clamp to the maximum."

## Decision

**Every formula that treats `QueueDepth.Value` as ops-denominated first checks that every sample in
its window actually carries `QueueDepthOps`, via one shared `queueUnitConsistent` helper. A window that
fails the check is treated as inconclusive, never coerced or best-effort interpreted:**

- `sustainedQueueAtOrAbove`/`sustainedQueueBelow` (Analyzer) return `false` for a non-ops window — a
  disk can't be confirmed saturated via queue depth, and can't be confirmed quiet either, from data the
  formula doesn't understand. `Underutilized` requires `sustainedQueueBelow == true`, so this fails
  scale-down closed, not open.
- `queueGrowthRate` (Analyzer) returns a new `(rate float64, valid bool)` instead of a bare `float64`.
  `Analysis.QueueGrowthRateValid` carries that flag to the Decider.
- `decider.demand()`'s open-workload branch is gated on `a.QueueGrowthRateValid` in addition to the
  existing `> queueGrowthThreshold` check. An invalid rate falls through to the closed-workload
  (latency-ratio) estimate — or to no estimate at all, ending in `NoAction`/`baseline_unavailable` — the
  same fallback path an ordinary "growth rate below threshold" already takes. The magnitude of an
  invalid rate is irrelevant; only validity decides whether it's used.

No coercion or normalization is attempted (the "explicit, testable workload/block-size model" the
issue's proposed direction also offers as an option) — there is no live path today that produces a
byte-denominated queue reading to normalize, so building that model now would be speculative. Failing
closed is the available, defensible choice; a normalized IOPS-equivalent conversion, if the product
ever wants platform's queue signal to drive demand too, is a design M7 can make with real data in hand.

## Options Considered

### Option A: Convert bytes to an IOPS-equivalent rate using a block-size assumption

Rejected for now: no workload block-size model exists anywhere in this codebase to convert a
byte-denominated *queue backlog* (not a bandwidth *rate*, which `bytesPerIOPS` already converts
elsewhere, ADR-008/ADR-016) into an operations count. Inventing one to convert a signal that isn't
produced by any implemented source yet is exactly the kind of speculative feature this codebase avoids
(CLAUDE.md's own don't-build-around-absence caveat cuts the other way here: there's nothing to build
around yet, and guessing a conversion model without data risks being wrong in a way that's worse than
simply not using the signal).

### Option B: Chosen — reject/fail-closed on unit mismatch, no conversion

### Option C: Leave the fields unchecked; treat this as purely test-authoring debt (add tests, no runtime guard)

Rejected. Tests alone would prove the current pipeline behaves correctly today, but the actual bug the
issue reports is a runtime property — the day `PlatformSource` starts producing `QueueDepthBytes`, tests
passing today say nothing about what the live formula does with the new data. The guard has to be in
the code path itself, matching the standing "reject unknown or unexpected units instead of silently
evaluating them" direction.

## Consequences

**Easier**
- `demand()`, `sustainedQueueAtOrAbove`, and `sustainedQueueBelow` can never silently misinterpret a
  differently-denominated queue reading — the ADR-001 risk is now enforced, not just documented.
- M7's `PlatformSource` lands into a Decider that already fails closed on its queue signal by default;
  wiring platform's queue metric into demand math (if ever done) is an explicit, reviewable change to
  lift this guard, not a latent bug waiting to be discovered.

**Harder**
- `queueGrowthRate`'s signature changed (`float64` → `(float64, bool)`), and `Analysis` gained
  `QueueGrowthRateValid` — a small ripple through every test/call site that constructs a growth-rate
  scenario by hand (`internal/decider/decider_test.go`).
- Platform's queue signal (once it exists) cannot inform the open-workload demand estimate at all until
  someone designs the conversion Option A rejected here — a real capability gap versus guest, on top of
  the ones ADR-001 already names (no read queue, byte-denominated).

**Revisit when**
- M7 ships `PlatformSource` and a real byte-denominated queue signal exists to design a conversion
  against, per Option A.
