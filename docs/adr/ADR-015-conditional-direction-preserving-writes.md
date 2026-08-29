# ADR-015: Both write paths are conditional on live state, never unconditional

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Closes:** GitHub issue #16 (P0)

## Context

A `decider.Decision` names a `Target` IOPS value, but that value is only meaningful relative to the
`Current` it was computed from — every guardrail in design.md §4.5/§4.6 (the ratio caps, the min-change
floor, the ceiling clamp) is expressed as an offset from `Current`, not as an absolute number chosen in
isolation. Before this fix, both write paths applied `Target` unconditionally:

- The independent-disk path (`UpdateIndependentDiskIOPS`) sent `spec.iops = Target` with no read of the
  disk's live state first, and no etag at all — `IndependentUpdateRequest` had no `Etag` field, and the
  disk API's own `metadata.etag` concurrency precondition (documented in the platform's OpenAPI spec)
  was never used.
- The dependent-disk path (`UpdateDependentDiskIOPS`) did re-read the VM immediately before writing
  (for its etag), but never compared the target disk's live current IOPS against `Decision.Current` —
  a fresh etag legitimized a write built from a stale decision just as readily as a stale one would
  have.

The concrete failure mode (from the issue): discovery observes 1000 IOPS; an operator or another
controller raises the disk to 5000; this agent, still holding the 1000 snapshot, computes
`scale_up: 1000 -> 2000` and writes the absolute value 2000 — silently *reducing* the real limit from
5000 to 2000, the opposite of what "scale-up-only" is supposed to guarantee under concurrent changes.
The same class of gap exists after the agent's *own* successful write: `Agent.actuate` never updated
`a.managed[].Disk.IOPS` after a write landed, so it stayed stale until the next `DiscoveryInterval`
tick — during which a second reconcile cycle could compute a decision from the pre-write value all over
again.

## Decision

**Both write paths re-read the target's live state immediately before writing, and abort — never
force — when what they find doesn't match what the Decision was computed from:**

```
live := fresh read (GetDisk for independent, GetVirtualMachine for dependent)
if live.etag == "":                                    abort — no concurrency precondition to write with
if live.current != decision.current:                    abort (ErrConcurrentModification)
if target crosses live.current in the wrong direction:  abort (ErrConcurrentModification)
```

The equality check is the primary gate; the direction check is kept as an independent second
assertion (not folded into the first) specifically so it keeps holding even if the equality check is
ever loosened to a tolerance band instead of strict equality — issue #16's own acceptance criterion
states the direction invariant on its own terms ("a scale-up request can never write a value below the
live current; future scale-down can never write above it"), not merely as a consequence of the equality
check.

The re-read's own etag is what is actually sent on the write — `IndependentUpdateRequest` gains an
`Etag` field, sent as `metadata.etag` on `UpdateDisk`, mirroring what `DependentUpdateRequest` already
did for the VM path. Never a value carried from an earlier `Discover` snapshot: the whole point of
optimistic concurrency is comparing against state no older than the write itself.

Both write functions (`UpdateIndependentDiskIOPS`, `UpdateDependentDiskIOPS`) now take an explicit
`expectedCurrent` parameter (`Decision.Current`, threaded through by the Actuator) alongside `target`.
`UpdateDependentDiskIOPS` also gains a `dryRun bool` parameter, replacing the Actuator's previous
special-cased early return for the dependent dry-run path — a dry run now runs every check above
(including the live-current comparison) against a fresh re-read and only skips the final mutating
`UpdateDependentDisk` call, so it reports a would-be conflict exactly like a real run would. The
independent path's existing `validateOnly` gets the same treatment for free, since the checks now run
before the `UpdateDisk` call regardless of `validateOnly`'s value.

`ErrConcurrentModification` is a new sentinel error in `internal/compute`, wrapped by both write paths'
conflict returns so callers can `errors.Is` it. `Agent.actuate` (`cmd/mws-iops-autoscaler/agent.go`)
special-cases it: on a conflict, it forces an immediate `discover()` rather than waiting up to
`DiscoveryInterval` for the next scheduled one, so the next reconcile cycle decides again from a
snapshot no older than the moment the conflict was detected — never a retry of the same stale target.
On success, `actuate` now also writes the verified applied value straight into `a.managed[]` itself,
rather than waiting for the next discovery to notice — closing the same class of staleness window for
the agent's own writes that the pre-write re-read closes for external ones.

## Options Considered

### Option A: Leave both paths unconditional, rely on the etag alone (dependent path only, status quo)

Rejected. The independent path had no etag at all, so it could not detect a conflict even in principle.
The dependent path's etag *did* detect a VM-level conflict (any field on the VM having changed), but
never checked the one thing that actually matters for this write's safety: whether the *specific disk's
current IOPS* — the value `Target` was computed relative to — had itself moved. An etag conflict is a
necessary signal but not a sufficient one; a VM's etag can be unchanged while its disks' `spec.iops`
still reflects exactly what discovery saw, which is the safe case this ADR's check is meant to confirm,
not merely infer from etag equality.

### Option B: Add the etag/current check only on the dependent path (skip the independent path)

Rejected — the issue's own worked example is specifically an independent-disk scenario (a standalone
disk's IOPS raised externally), and CLAUDE.md documents `metadata.etag` as the concurrency precondition
for `UpdateDisk` just as it does for `UpdateVirtualMachine`. Leaving the independent path unconditional
would have fixed the less risky path (VM writes touch every sibling disk's collection entry) and left
the more common one (a standalone data disk, the primary target of this whole project) exposed.

### Option C: Tolerance band instead of strict equality (e.g., allow live within ±N% of expected)

Rejected for this pass. A tolerance band trades a hard safety guarantee for reduced write-conflict
churn in exchange for reasoning that doesn't obviously hold — what tolerance is safe depends on how far
off a stale `Target` can drift before its own guardrail math (ratio caps, min-change floor) stops
producing something sensible, which nothing in this codebase has characterized yet. Strict equality is
simple, provably safe, and cheap: a conflict just means "rediscover and decide again next cycle," not a
lost opportunity requiring compensation. `directionPreserved`'s independent check is what would make a
future tolerance band safe to add later without reopening this ADR's core guarantee.

### Option D: Strict equality + independent direction check (chosen)

Matches the issue's acceptance criteria exactly: a hard concurrency gate for the common case, plus a
second, independently-reasoned safety net for the specific invariant ("never write past live current in
the wrong direction") that the issue calls out on its own terms.

## Consequences

**Easier**
- The scale-up-only safety guarantee (Phase 1, ADR-014-adjacent) now actually holds under concurrent
  external changes, not just under the assumption that nothing else touches the disk between discovery
  and write.
- A conflict is diagnosable: `ErrConcurrentModification`-wrapped errors name both the expected and live
  values, and `Agent.actuate`'s forced rediscovery means the very next cycle already has fresh data
  rather than compounding the staleness.

**Harder**
- Every write now costs one extra read (`GetDisk` before `UpdateDisk`, on top of the dependent path's
  pre-existing `GetVirtualMachine` read) — acceptable given writes happen at most `MaxChangesPer24h`
  times per disk, not on every reconcile cycle.
- `IndependentUpdateRequest`/`UpdateIndependentDiskIOPS`/`UpdateDependentDiskIOPS` all gained parameters
  — every existing call site and test fixture needed updating to supply a matching `expectedCurrent`
  (previously untested — `Decision.Current` was informational only before this ADR).

**Revisit when**
- A future write-conflict-rate metric shows the strict-equality gate is producing enough rediscovery
  churn in practice to justify Option C's tolerance band — `directionPreserved`'s independent check
  already keeps the direction guarantee intact if that day comes.

## Action Items

1. [x] `internal/compute/types.go`: `Disk.Etag`.
2. [x] `internal/compute/client.go`: `IndependentUpdateRequest.Etag`.
3. [x] `internal/compute/sdk_client.go`: `GetDisk` populates `Etag`; `UpdateIndependentDisk` sends
       `metadata.etag`, rejects an empty one locally, and distinguishes an etag-conflict response.
4. [x] `internal/compute/write.go`: `ErrConcurrentModification`; both write functions re-read live
       state, compare against `expectedCurrent`, and check `directionPreserved`; `UpdateDependentDiskIOPS`
       gains `dryRun`.
5. [x] `internal/actuator/actuator.go`: threads `d.Current` into both write paths; the dependent
       dry-run special case is removed in favor of `dryRun` flowing through to `write.go`.
6. [x] `cmd/mws-iops-autoscaler/agent.go`: `actuate` forces rediscovery on `ErrConcurrentModification`
       and adopts a successful write's verified value into `a.managed[]` immediately.
7. [x] Tests: conflict/direction/dry-run-catches-conflict/empty-etag cases in
       `internal/compute/write_test.go`; a direct `directionPreserved` table test; matching Actuator-level
       tests; Agent-level tests proving a conflict forces rediscovery and never adopts an unverified
       value, and that a success adopts its value immediately.
8. [x] `docs/design.md` §4.7 synced.

> **Refined (2026-08-29, pre-merge review): the forced rediscovery must end the whole cycle, not just
> the one disk's write.** `Agent.actuate`'s forced `discover()` on `ErrConcurrentModification` replaces
> `a.managed`/`a.iopsPool` — but `reconcile()` had already snapshotted both once at the top of the
> cycle, and every disk not yet processed (sibling p95s, VM headroom, its own `Decision`) was computed
> from that now-superseded snapshot. The loop kept going regardless, deciding and potentially writing
> for every remaining disk against data the platform had already moved past.
>
> `Agent.generation` (an integer bumped only alongside a successful discovery, in the same critical
> section as `managed`/`iopsPool`) closes this: `reconcile()` snapshots it at the top exactly like
> `managed`/`iopsPool`, and re-checks it after every `actuate()` call — the only place it can change
> mid-cycle. A mismatch aborts the rest of the cycle immediately; the next reconcile tick decides fresh
> from the new snapshot instead. `vm_cap_reached` is not published for an aborted cycle, since an
> `anyVMCapReached` aggregated from only part of the managed set could be a false negative — worse than
> not publishing at all. This generation counter is the same mechanism ADR-014's own refinement uses for
> `managedValid`; both are set together in `discover()`'s single success critical section.

9. [x] `cmd/mws-iops-autoscaler/agent.go`: `Agent.generation`; `reconcile()` snapshots it and aborts the
       remaining loop iterations (skipping `RecordVMCapReached`) the moment it changes.
10. [x] Tests: a two-disk cycle where the first disk's conflict forces a rediscovery proves the second
        disk's `Actuator.Apply` is never reached.
