# ADR-019: Incarnation safety extended to the write precondition, guest counters, and the latency baseline

**Status:** Accepted
**Date:** 2026-08-30
**Deciders:** Product Lead (NBS)

## Context

ADR-018 made the agent's cross-cycle *state* — the Sampler ring buffer, the cooldown clock, and the
24h change budget — key on `DiskName + IncarnationID` (`IdentityKey()`) rather than `DiskName` alone,
so a disk deleted and recreated under the same name (an ordinary infra-as-code "replace", CLAUDE.md
"Disk resource identity") starts each of those systems clean. That pass deliberately scoped itself to
those three state stores and named two exposures it left open:

1. Its own "Revisit when" flagged the **Analyzer's latency baseline**
   (`WindowAnalyzer.baselines`/`established`, keyed by `Analyze`'s plain `diskID` parameter) as having
   "the identical structural exposure — a recreated disk could inherit a stale latency baseline the
   same way it used to inherit stale samples/cooldown/budget state."
2. Two state stores keyed by bare identity were never in ADR-018's frame at all:
   - **`GuestSource.prev`/`prevTime`** — the `/proc/diskstats` cumulative-counter baselines the guest
     metric source deltas against — was keyed by `DiskRef.DiskID`. A same-name replacement would make
     the source compute a delta *across* the replacement boundary (two unrelated counter series), and
     a disk that simply left the managed set for a cycle kept its stale baseline for a later reattach.
   - The **write path itself** (`UpdateIndependentDiskIOPS`/`UpdateDependentDiskIOPS`) had no notion of
     incarnation. Its optimistic-concurrency precondition (ADR-015) compares *live IOPS* against the
     Decision's `expectedCurrent` — but a replacement disk can sit at the very same IOPS as the stale
     Decision's `Current`, so the check passes and the write silently mutates a **different resource**.
     `reconcilePendingBudget`/`resolveReservationAfterWriteFailure` had the mirror-image gap: they
     resolved a reservation from a live read that could be a *different* incarnation than the one the
     reservation belonged to.

A regression-test pass added after PR #21 encoded all of these as failing tests, alongside two
adjacent control-loop-safety gaps and one config-validation gap (below). This ADR records the fixes.

## Decision

**1. The write path is incarnation-aware.** `actuator.Target` gains `IncarnationID`, threaded from the
managed disk into both write functions as a new `expectedIncarnation` precondition. Immediately before
the IOPS comparison, each write path checks the *live-read* incarnation against the expected one; a
mismatch returns the new sentinel `compute.ErrIncarnationMismatch` **before any mutating request is
sent**. Like `ErrConcurrentModification` this is a pre-write refusal, but it is a distinct error
because the remedy differs: the targeted resource no longer exists, so there is nothing to retry
against. `Agent.actuate` releases the budget reservation (nothing was written) and returns without
forcing a rediscovery — the next scheduled discovery onboards the replacement with its own fresh
identity and sampling.

The check **degrades gracefully**, exactly as `IdentityKey()` does: a mismatch is asserted only when
both the expected and the live incarnation are non-empty (`compute.incarnationMatches`). An absent
`metadata.id` on either side falls back to the pre-existing IOPS-only precondition rather than refusing
every write.

**2. Live-state resolution verifies incarnation.** `Agent.liveCurrentIOPS` (used by both
`reconcilePendingBudget` and `resolveReservationAfterWriteFailure`) now fails closed when the live
resource's incarnation differs from the reservation's — routing into the existing "leave it pending"
branch instead of resolving a reservation against a coincidentally-equal replacement's IOPS. ADR-018
already closed the *keying* half of this (a stale reservation from incarnation A no longer *matches* a
managed record for incarnation B); this closes the TOCTOU half, where the managed record and the live
read momentarily disagree across a replacement.

**3. The guest metric source keys counters by `IdentityKey()` and drops departed disks.**
`GuestSource.prev`/`prevTime` key on `IdentityKey()`, so a new incarnation cold-starts rather than
deltaing across the replacement boundary. `Sample` also prunes any counter baseline whose key is not
in the current managed set — the same prune-on-absence contract `Sampler.SetDisks` already follows —
so a disk that disappears and later reattaches cold-starts rather than computing a delta against a
stale, arbitrarily old counter reading.

**4. The Analyzer's latency baseline keys on `IdentityKey()`.** `reconcile` passes
`m.Disk.IdentityKey()` (not the bare `DiskName`) as `WindowAnalyzer.Analyze`'s state key, so a
recreated disk establishes its own baseline from its own samples instead of inheriting the previous
incarnation's frozen one — completing ADR-018's named follow-up. Display-facing identifiers are
untouched, consistent with ADR-018: `Decision.DiskID`, the journal `disk` field, and every Prometheus
`disk` label still use plain `DiskName`; only the analyzer's internal state key becomes the composite.

## Adjacent fixes recorded in the same change

These came from the same regression pass and are not incarnation-specific, but ship together.

- **A pre-write refusal never consumes change budget.** `ErrConcurrentModification` (and now
  `ErrIncarnationMismatch`) prove the mutating request was never sent, so `actuate` now **releases**
  the reservation outright rather than resolving it from a live read. Previously the conflict path fell
  through `resolveReservationAfterWriteFailure`, whose ambiguous branch could leave a reservation
  pending for a change that provably never happened — silently spending a budget slot. This tightens,
  and does not weaken, ADR-013/ADR-015: a genuine *post*-send write error still resolves from live
  state exactly as before.
- **A failed forced rediscovery aborts the reconcile cycle.** On `ErrConcurrentModification`, `actuate`
  forces a rediscovery. If that rediscovery *succeeds*, `generation` bumps and `reconcile` already
  aborted the rest of the cycle (the snapshot it decided against is superseded). If it *fails*,
  `invalidateManaged()` clears `managedValid` instead, and `reconcile` now treats that identically —
  the snapshot is suspect, so the remaining disks this cycle are not actuated against it. Extends
  ADR-014's fail-closed principle to the mid-cycle conflict path.
- **`collection_interval` must be strictly shorter than every evaluation window.**
  `validateCollectionIntervalAgainstWindows` rejected only `interval > window`; it now rejects
  `interval >= window`. At exactly window cadence a trailing span of that length holds exactly two real
  samples — one short of `analyzer.minSamplesForWindow` (3) — and the boundary sample leaves the span
  at exactly `window`, so `trailingWindow` never reaches back for a third. Such a config is accepted but
  can never produce an evaluable window; it is now rejected at validation, per the regression's own
  allowance ("reject `collection_interval == scale_up_window` during validation, or make the accepted
  configuration actually produce an evaluable window"). A strictly shorter interval remains valid — a
  natural or reached-back predecessor supplies the third sample.

## Options Considered

### Option A: Pre-flight incarnation read in `actuate`, before the actuator

Rejected. `actuate`'s own client is not the actuator's, and a second read opens a TOCTOU window the
write path's own fresh re-read exists to close. Threading `expectedIncarnation` into the write path
reuses the read it already makes immediately before the write — no extra API call, no new race.

### Option B: Reuse `ErrConcurrentModification` for the incarnation mismatch

Rejected. The two share downstream handling (pre-write refusal, release the reservation) but differ in
remedy: a concurrent modification is worth an immediate rediscovery-and-retry against the *same* disk;
a replacement is not (there is nothing to retry against). A distinct sentinel keeps the log honest and
lets `actuate` skip the needless rediscovery on the replacement path.

### Option C: Threaded `expectedIncarnation` precondition + distinct sentinel (chosen)

Matches the actual risk (a write must prove it is touching the same resource the Decision was computed
for) with no extra read, and keeps the incarnation-mismatch and concurrent-modification remedies
separate.

## Consequences

**Easier**
- A stale Decision can no longer mutate, and a stale reservation can no longer resolve against, a
  same-name replacement disk — the last two paths where a recreate was still invisible.
- A recreated disk starts its latency baseline and its guest counter deltas clean, matching the
  samples/cooldown/budget guarantee ADR-018 already gave.

**Harder**
- `UpdateIndependentDiskIOPS`/`UpdateDependentDiskIOPS` gained an `expectedIncarnation` parameter, and
  `validateDependentWriteTarget` now also returns the target's incarnation — every call site and test
  fixture supplies `""` where no incarnation is under test (the graceful-degradation path), so existing
  behavior is unchanged for them.
- Hand-built `ManagedDisk` fixtures that feed real samples through the Analyzer now need a resolved
  policy (real evaluation windows), which every production disk already has via `registry.Select`. The
  shared `independentDisk()`/`dependentDisk()` test helpers gained one; fixtures that never analyze are
  unaffected.

**Revisit when**
- `metadata.id` is verified against a live API response in this project (still SDK-only, as ADR-018
  noted) — the incarnation precondition's assumptions should be re-checked then.

## Action Items

1. [x] `internal/compute/write.go`: `ErrIncarnationMismatch`, `incarnationMatches`; both write
       functions take `expectedIncarnation` and refuse a mismatch before the mutating call;
       `validateDependentWriteTarget` returns the target's incarnation.
2. [x] `internal/actuator/actuator.go`: `Target.IncarnationID`, threaded into both write paths.
3. [x] `cmd/mws-iops-autoscaler/agent.go`: `actuate` sets `Target.IncarnationID`, releases the
       reservation on any pre-write refusal, handles `ErrIncarnationMismatch` distinctly;
       `liveCurrentIOPS` verifies incarnation (`verifyLiveIncarnation`); `reconcile` aborts on a
       failed forced rediscovery (`!managedValid`) and keys `Analyze` on `IdentityKey()`.
4. [x] `internal/metrics/guest.go`: `prev`/`prevTime` keyed by `IdentityKey()`; `Sample` prunes
       baselines for disks no longer in the managed set.
5. [x] `internal/config/validate.go`: `validateCollectionIntervalAgainstWindows` rejects
       `interval >= window`.
6. [x] Tests: the post-PR21 regression suite (`*/review_regression_test.go`) now passes; the stale
       `validateCollectionIntervalAgainstWindows` equal-to-window "valid" assertion updated to
       "rejected"; write-path fixtures supply `expectedIncarnation`; `independentDisk()`/
       `dependentDisk()` carry a resolved policy.
7. [x] `docs/adr/ADR-018`: its "Revisit when" analyzer-baseline item marked addressed here.
