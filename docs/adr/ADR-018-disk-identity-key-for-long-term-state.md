# ADR-018: Samples, cooldown, and the change budget key on DiskName + IncarnationID, not DiskName alone

**Status:** Accepted
**Date:** 2026-08-30
**Deciders:** Product Lead (NBS)

## Context

Every piece of state this agent carries across reconcile cycles — the Sampler's ring buffer
(`internal/metrics.Sampler`), the Decider's cooldown clock (`internal/state.Tracker`), and the 24h
change budget (`internal/state.Budget`) — was keyed by `DiskName` alone (the cloud resource name,
`compute.StorageDisk.DiskName`). `Sampler.SetDisks`'s own doc comment already flagged half the risk:
"a disk that was detached and a different one reattached under the same alias must not silently
inherit stale history" — but that protection only fires when a disk actually *leaves* the managed set
under one identity and a *different* disk appears under another. It does nothing for the case where the
same `DiskName` string keeps appearing, cycle after cycle, because the platform allows a name to be
reused: `DiskName` is the disk's `name` field, which is **user-settable** for an independent disk
(Terraform's `mws_compute_disk.disk` field is required, chosen by the caller — CLAUDE.md "Disk resource
identity"). Deleting a disk and creating a new one under the identical name is an ordinary
infra-as-code "replace," not a contrived edge case.

Under the pre-fix keying, that replace is invisible to every long-term-state system in this agent. The
Sampler keeps appending the new disk's samples to the old disk's ring buffer — a scale-up/scale-down
window built from a mix of two unrelated disks' I/O history. The Tracker's cooldown clock, started by
the old disk's last change, blocks the new disk from being touched for no reason connected to the new
disk at all. Worst, `state.Budget`'s pending-reservation reconciliation (`reconcilePendingBudget`,
ADR-013) could resolve a reservation left over from the *old* disk's crash-interrupted write by reading
the *new* disk's live IOPS — a coincidental match confirms or releases a change that has nothing to do
with what's actually being verified.

CLAUDE.md's own SDK research settles what the stable identifier actually is: `GetDisk`'s response
carries `metadata.id`, and the SDK's own generated comment on the `name` field reads *"Deprecated:
отказываемся в пользу metadata.id"* — the platform's own stated direction is away from name as
identity. `metadata.id` is assigned at creation and is not reused.

## Decision

**`compute.StorageDisk` (and `metrics.DiskRef`, which mirrors it for the Sampler) gain an
`IncarnationID` field — `metadata.id`, read from the same `GetDisk` call `Discover` already makes for
every managed disk of either class, no new API call.** Both types gain an `IdentityKey()` method:
`DiskName + "\x00" + IncarnationID`. The Sampler's ring buffer, the Tracker's cooldown map, and the
Budget's `ChangeEvent.DiskID` now key on `IdentityKey()`, not bare `DiskName`.

`DiskName` itself is combined into the key, not replaced by `IncarnationID` alone, for two reasons:
readability (a human grepping `budget.jsonl` can still recognize which disk an entry is about) and
graceful degradation (see below).

**Display-facing identifiers are unchanged.** `decider.Decision.DiskID`, the decision journal's `disk`
field, and every Prometheus `disk` label keep using plain `DiskName` — `Decide`'s own `diskID` parameter
never becomes the composite key. Only the *lookup calls* into Sampler/Tracker (`Samples`, `State`,
`RecordChange`, `ReserveChange`, `BeginResize`/`EndResize`) use `IdentityKey()`; the values those calls
return or produce (`Analysis`, `GuardrailState`, `Decision`) carry no identity of their own and flow
into `Decide`/the journal exactly as before. This keeps operator-facing output legible — nobody wants
`disk="vm-1-data\x00a1b2c3-..."` in a Grafana panel — while the actual state-machine keys underneath are
incarnation-safe.

**A missing `metadata.id` degrades, it does not exclude.** Unlike `spec.iops <= 0` (issue #4, a genuine
data-integrity problem), an absent `IncarnationID` is not evidence of a broken disk — every real disk
this project has access to returns one — so `Discover` leaves the field empty rather than skipping the
disk. `IdentityKey()` in that case is just `DiskName + "\x00"`, i.e. the exact pre-fix behavior for that
one disk. This also means the fix requires zero changes to the many existing tests that construct a
`StorageDisk`/`ManagedDisk` by hand without ever setting `IncarnationID` — they keep working exactly as
before, since they never exercise the specific delete-and-recreate scenario this ADR protects against.

## Options Considered

### Option A: Key everything on `IncarnationID` alone

Rejected. Loses human-readability in the budget log and anywhere else the key surfaces for debugging,
for no correctness benefit over the combined form — `DiskName + IncarnationID` is exactly as collision-safe.

### Option B: Detect a recreate explicitly and clear state, rather than changing the key

Considered: have `Discover` compare each disk's `IncarnationID` against what was seen last cycle and
proactively purge Sampler/Tracker/Budget state for a changed one. Rejected as more code for an
equivalent outcome — `SetDisks` already drops a ring buffer entry the instant its key stops appearing
in the managed set (existing behavior, reused as-is here); changing the key achieves the identical
purge automatically, the moment the new incarnation's key differs from the old one, with no new
detection logic to get wrong.

### Option C: Combined `DiskName + IncarnationID` key, display fields unchanged (chosen)

Matches the actual risk (long-term *state* must not survive a recreate) without touching anything a
human or a dashboard reads, and needs no new detection logic — `SetDisks`'s existing prune-on-absence
behavior does the work.

## Consequences

**Easier**
- A disk deleted and recreated under the same name starts its scale-up/scale-down window, cooldown
  clock, and change-budget count clean — no history from an unrelated resource that happens to share a
  name.
- `reconcilePendingBudget`'s startup reconciliation (ADR-013) can no longer resolve a stale reservation
  against a same-named replacement disk's live IOPS — a coincidental match that used to be silently
  treated as meaningful now correctly falls through to "not managed, left pending."

**Harder**
- `Sampler.Samples`'s signature changed from `Samples(diskID string)` to `Samples(ref DiskRef)` — every
  call site needed the caller to have a full `DiskRef`, not just a name, in hand. `Agent.diskRef(m)` is
  the one place this construction happens now, so every caller derives it identically.
- Every existing test that asserted against `Tracker.State("vm-1-data", ...)` or similar needed
  updating to query with the same `IdentityKey()` the production code now writes under — mechanical,
  but touches a fair number of call sites.

**Revisit when**
- `metadata.id` is verified against a live API response in this project (CLAUDE.md still marks this
  SDK-only) — if the field's real-world behavior differs from the generated model in some way, this
  decision's assumptions should be re-checked against that.
- The Analyzer's own latency baseline (`WindowAnalyzer.baselines`/`established`, keyed by plain
  `diskID` in `Analyze`'s own parameter) has the identical structural exposure — a recreated disk could
  inherit a stale latency baseline the same way it used to inherit stale samples/cooldown/budget state.
  Left unaddressed in this pass (out of the reviewed scope, which named samples/cooldown/budget
  specifically) — a natural next candidate for the same fix, not a discovered-and-ignored gap.

## Action Items

1. [x] `internal/compute/types.go`: `StorageDisk.IncarnationID`, `StorageDisk.IdentityKey()`,
       `Disk.IncarnationID`.
2. [x] `internal/compute/sdk_client.go`: `GetDisk` reads `metadata.id` into `Disk.IncarnationID`.
3. [x] `internal/compute/discovery.go`: `Discover` copies `IncarnationID` from the same `GetDisk` call
       that already reads `SpecIOPS`, for both disk classes uniformly.
4. [x] `internal/metrics/types.go`: `DiskRef.IncarnationID`, `DiskRef.IdentityKey()`.
5. [x] `internal/metrics/sampler.go`: ring buffer keyed by `IdentityKey()`; `SetDisks`'s prune logic
       and `Tick`'s DiskID→DiskRef lookup updated to match; `Samples` takes a `DiskRef`.
6. [x] `cmd/mws-iops-autoscaler/agent.go`: new `Agent.diskRef(m)` helper; every Sampler/Tracker call
       site (`reconcile`, `actuate`, `resolveReservationAfterWriteFailure`, `reconcilePendingBudget`)
       uses `IdentityKey()`; `Decision.DiskID`/journal/telemetry untouched.
7. [x] Tests: `internal/compute`'s `IdentityKey()` collision/stability table tests and a `Discover`
       propagation test; `internal/metrics`'s Sampler-level recreate scenario (same DiskID, different
       IncarnationID starts empty) and the Tick-routes-by-current-incarnation case;
       `cmd/mws-iops-autoscaler`'s `reconcilePendingBudget` recreate scenario (a stale reservation never
       matches the replacement disk's live state); every pre-existing Tracker-state assertion updated
       to query by `IdentityKey()`.
8. [x] `CLAUDE.md`: new "Disk resource identity" section documenting the SDK's own `metadata.id`
       field and its `name`-is-deprecated comment.
9. [x] `docs/design.md` §4.1 and §5.2 synced.
