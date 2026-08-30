# ADR-019: Incarnation safety extended to the write precondition, guest counters, and the latency baseline

**Status:** Accepted
**Date:** 2026-08-30
**Deciders:** Product Lead (NBS)

## Context

ADR-018 made the agent's cross-cycle *state* — the Sampler ring buffer, the cooldown clock, and the
24h change budget — key on `DiskName + IncarnationID` (`IdentityKey()`) rather than `DiskName` alone,
so a disk deleted and recreated under the same name (an ordinary infra-as-code "replace", docs/platform-reference.md
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

**Amendment:** a second review pass against the first version of this fix found it incomplete in two
places, both now corrected in the text below and reflected in the Action Items. Recorded here rather
than silently rewritten, since the gaps are instructive about why "the same shape of fix" does not
always transfer between the two disk classes:

- The dependent-path incarnation check (point 1) originally compared against
  `target.IncarnationID` read off the VM's own `StorageDisk` entry — but `translateVMResponse` never
  populates that field from a VM read at all (see point 1's detail below), so the comparison was always
  against an empty string and `incarnationMatches`' original (symmetric) degrade-when-either-side-is-empty
  rule silently passed every check. The dependent write path was therefore not actually incarnation-safe
  in production, only in a test fixture that (incorrectly) set the field by hand. Fixed by having the
  dependent path make its own fresh `GetDisk` read, and by making the degrade rule asymmetric (fail
  closed when the *expected* side is known but the *live* side unexpectedly is not).
- `ErrIncarnationMismatch` originally did not force a rediscovery, on the reasoning that "there is
  nothing to retry against." That reasoning addressed only the replaced disk itself — it missed that
  the whole reconcile cycle's snapshot (every sibling's IOPS-pool headroom, `siblingP95IOPS`) is exactly
  as stale as it would be after a concurrent-modification conflict, which *does* force a rediscovery for
  that reason. Fixed by point 5 below.

## Decision

**1. The write path is incarnation-aware — and the two disk classes need genuinely different reads to
prove it.** `actuator.Target` gains `IncarnationID`, threaded from the managed disk into both write
functions as a new `expectedIncarnation` precondition.

- **Independent path** (`UpdateIndependentDiskIOPS`): the disk's own `GetDisk` re-read — already made
  immediately before the write, for the pre-existing IOPS precondition — carries `metadata.id`
  directly. The incarnation check reuses that same read; no extra API call.
- **Dependent path** (`UpdateDependentDiskIOPS`): **cannot** reuse its own pre-write re-read the same
  way. That re-read is `GetVirtualMachine`, and `sdk_client.go`'s `translateVMResponse` — the one place
  a VM response becomes `StorageDisk` values — never populates `IncarnationID` from a VM read, for
  either disk class (docs/platform-reference.md: *"`StorageDiskStatusResponse`... does not carry `metadata.id`"*). Only
  a separate `GetDisk` call does. So this path makes that call itself, immediately before the write,
  using the target attachment's own `DiskName` (`validateDependentWriteTarget` returns it) — the exact
  same call `Discover` already makes per managed disk (discovery.go), just repeated here so the check is
  never older than the write itself. Skipped entirely when `expectedIncarnation` is empty, since there
  is nothing to compare against and no reason to pay for a read whose result would be discarded anyway.

Either way, a mismatch returns the new sentinel `compute.ErrIncarnationMismatch` **before any mutating
request is sent**. Like `ErrConcurrentModification` this is a pre-write refusal, but it is a distinct
error because the remedy differs: the targeted resource no longer exists, so there is nothing to retry
*against that resource* — but see point 5 below on why `actuate` still forces a rediscovery.

The check degrades **asymmetrically**, not the same way both directions:

- `expectedIncarnation == ""` (Discover itself had no `metadata.id` for this disk) — nothing to compare
  against, so this falls back to the pre-existing IOPS-only precondition, mirroring
  `StorageDisk.IdentityKey`'s own "degrade gracefully when unavailable" contract.
- `expectedIncarnation != ""` but the live read comes back empty — **fails closed**, not treated the
  same as the case above. An expected value existing at all means Discover's own `GetDisk` successfully
  read a `metadata.id` for this disk earlier; every real disk this project has access to returns one
  (ADR-018). A live read that suddenly comes back empty is not "unavailable," it is unexplained, and
  assuming it is safe would reopen exactly the gap this fix closes.
- Both non-empty: exact match required.

**2. Live-state resolution verifies incarnation.** `Agent.liveCurrentIOPS` (used by both
`reconcilePendingBudget` and `resolveReservationAfterWriteFailure`) now fails closed when the live
resource's incarnation differs from the reservation's — routing into the existing "leave it pending"
branch instead of resolving a reservation against a coincidentally-equal replacement's IOPS. ADR-018
already closed the *keying* half of this (a stale reservation from incarnation A no longer *matches* a
managed record for incarnation B); this closes the TOCTOU half, where the managed record and the live
read momentarily disagree across a replacement.

**3. The guest metric source keys counters by `IdentityKey()` and drops departed disks — regardless of
whether this cycle's own read succeeds.** `GuestSource.prev`/`prevTime` key on `IdentityKey()`, so a new
incarnation cold-starts rather than deltaing across the replacement boundary. `Sample` prunes any
counter baseline whose key is not in the current managed set *before* it ever opens
`/proc/diskstats` — not after a successful open+parse, which would leave a departed disk's stale
baseline in place on any cycle where the read itself also happened to fail (a transient permissions
glitch, the file momentarily missing) — unrelated to, but coincident with, the disk's own departure.
The prune only needs the caller-supplied managed set, never the read's own result, so there is no
reason to gate it on that read succeeding.

**4. The Analyzer's latency baseline keys on `IdentityKey()`, with the display-facing `DiskID` restored
afterward.** `reconcile` passes `m.Disk.IdentityKey()` (not the bare `DiskName`) as
`WindowAnalyzer.Analyze`'s single `diskID` parameter — which doubles as both the analyzer's internal
baseline-map key and the value `Analyze` copies verbatim into the returned `Analysis.DiskID` field
(`analyzer.go`: `out := Analysis{DiskID: diskID}`). Passing the composite key satisfies the first job
but corrupts the second: an exported field meant to be human-readable would carry the `"\x00"`-joined
form. `reconcile` corrects this itself, one line after the call — `an.DiskID = diskID` (the plain
name) — rather than changing the Analyzer package's public signature, since nothing downstream reads
`Analysis.DiskID` today (`Decide`'s own `diskID` parameter is passed separately, from the same plain
value) and every other analyzer test call site passes one identical string for both roles already.
This completes ADR-018's named follow-up while keeping display-facing identifiers untouched, consistent
with ADR-018: `Decision.DiskID`, the journal `disk` field, and every Prometheus `disk` label all still
use plain `DiskName`.

**5. An incarnation mismatch invalidates the reconcile cycle's snapshot, exactly like a concurrent-modification conflict.**
`Agent.actuate`'s `ErrIncarnationMismatch` branch releases the reservation (nothing was written) *and*
forces a rediscovery — not to retry against the disk this Decision targeted (that resource is gone,
so there is nothing to retry), but because the whole snapshot this reconcile cycle is working from is
now proven wrong for at least this one disk. Every sibling's `VMState` (`siblingP95IOPS`, the VM's IOPS
pool) was computed from the same managed set captured at the top of the cycle, so it is exactly as
suspect as it would be after a concurrent-modification conflict — `reconcile`'s existing
generation/`managedValid` check (added for that conflict path) aborts the rest of the cycle either way,
with no separate abort logic needed for this branch.

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
reuses the read it already makes immediately before the write for the independent path — no extra API
call, no new race. The dependent path still ends up making its own extra `GetDisk` call (point 1), but
that call is made *inside* `UpdateDependentDiskIOPS` itself, immediately before the write — the same
"no older than the write" property Option A would have sacrificed by reading from `actuate`, one layer
higher and one write-decision earlier.

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
  `validateDependentWriteTarget` now returns the target attachment's `DiskName` (not an incarnation —
  the VM read never carries one) so the dependent path can make its own fresh `GetDisk` call. Every call
  site and test fixture supplies `expectedIncarnation=""` where no incarnation is under test (the
  graceful-degradation path, which also skips that extra call), so existing behavior is unchanged for
  them.
- A dependent-disk write now costs one additional `GetDisk` read whenever `expectedIncarnation` is
  known — on top of the pre-existing `GetVirtualMachine` read — the same trade-off ADR-015 already
  accepted for the independent path's own pre-write re-read: acceptable given writes happen at most
  `MaxChangesPer24h` times per disk, not every reconcile cycle.
- Hand-built `ManagedDisk` fixtures that feed real samples through the Analyzer now need a resolved
  policy (real evaluation windows), which every production disk already has via `registry.Select`. The
  shared `independentDisk()`/`dependentDisk()` test helpers gained one; fixtures that never analyze are
  unaffected.
- A regression-test fixture that hand-set `IncarnationID` directly on a VM-read `StorageDisk` (modeling
  the dependent-disk replacement scenario) had to be rebuilt to route the incarnation instead through a
  separate fake `GetDisk` response — matching what `translateVMResponse` actually produces. The original
  fixture would not have caught the amendment's own gap.

**Revisit when**
- `metadata.id` is verified against a live API response in this project (still SDK-only, as ADR-018
  noted) — the incarnation precondition's assumptions should be re-checked then.

## Action Items

1. [x] `internal/compute/write.go`: `ErrIncarnationMismatch`; `incarnationMatches` with the asymmetric
       degrade rule (expected empty → allow; expected known + live empty → fail closed); both write
       functions take `expectedIncarnation`. Independent path checks it against its own existing
       `GetDisk` re-read. `validateDependentWriteTarget` returns the target attachment's `DiskName`
       (never an incarnation — the VM read carries none); `UpdateDependentDiskIOPS` makes its own
       fresh `GetDisk` call against that name to read the incarnation, skipped when
       `expectedIncarnation` is empty. Either path refuses before the mutating call.
2. [x] `internal/actuator/actuator.go`: `Target.IncarnationID`, threaded into both write paths.
3. [x] `cmd/mws-iops-autoscaler/agent.go`: `actuate` sets `Target.IncarnationID`, releases the
       reservation on any pre-write refusal (both `ErrConcurrentModification` and
       `ErrIncarnationMismatch`), and forces a rediscovery on **either** — an incarnation mismatch
       invalidates the whole cycle's snapshot the same way a conflict does, not just the one disk
       that triggered it. `liveCurrentIOPS` verifies incarnation (`verifyLiveIncarnation`);
       `reconcile` aborts on a failed forced rediscovery (`!managedValid`) from either error; `Analyze`
       is keyed on `IdentityKey()` with the plain `diskID` restored onto `an.DiskID` immediately after
       the call, so the returned `Analysis.DiskID` stays human-readable.
4. [x] `internal/metrics/guest.go`: `prev`/`prevTime` keyed by `IdentityKey()`; `Sample` prunes
       baselines for disks no longer in the managed set *before* opening `/proc/diskstats`, so the
       prune runs even on a cycle where that read fails.
5. [x] `internal/config/validate.go`: `validateCollectionIntervalAgainstWindows` rejects
       `interval >= window`.
6. [x] Tests: the post-PR21 regression suite (`*/review_regression_test.go`) now passes; the stale
       `validateCollectionIntervalAgainstWindows` equal-to-window "valid" assertion updated to
       "rejected"; write-path fixtures supply `expectedIncarnation`; `independentDisk()`/
       `dependentDisk()` carry a resolved policy; a dependent-disk regression fixture rebuilt to route
       its incarnation through a fake `GetDisk` response rather than setting it directly on a VM-read
       `StorageDisk`; new tests cover the dependent path's fresh-read mismatch/match/empty-live/
       read-failure cases, the incarnation-mismatch cycle-abort, the restored `Analysis.DiskID`, and
       the guest-source prune surviving a failed diskstats read.
7. [x] `docs/adr/ADR-018`: its "Revisit when" analyzer-baseline item marked addressed here.
