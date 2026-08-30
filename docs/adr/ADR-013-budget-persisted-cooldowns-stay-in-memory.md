# ADR-013: The 24h change budget must be persisted; cooldowns may safely stay in-memory

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Amends:** `docs/design.md` §9.4's "State persistence" deferred decision, which lumped the budget in
with cooldowns under one argument that only actually covers cooldowns

## Context

§9.4 deferred all state persistence with one justification: "v1 keeps cooldowns and budgets in
memory, so a restart forgets them. Bounded by the cold-start rule (no action until windows refill),
which is at least as long as a cooldown. Revisit if restarts prove frequent."

That argument is sound for cooldowns and was, until this ADR, only *conditionally* true: it depends
on `cooldown.scale_up ≤ scale_up_window` and `cooldown.scale_down ≤ scale_down_window` always
holding, so that the cold-start wait after a restart is never shorter than the cooldown a restart
would forget. `internal/config/validate.go` enforced this for `scale_up` only
(`cooldown.scale_up > scale_up_window` rejected, "the agent could never act") — there was no
symmetric check for `scale_down`, so a config with `cooldown.scale_down: 1h` and
`scale_down_window: 30m` passed validation, and on that config a restart genuinely could shorten the
effective scale-down cooldown below what was configured. This ADR closes that gap (see Action Items)
so the cold-start-bound argument is unconditionally true for both directions, not just scale-up.

The same argument **does not apply to the 24h change budget at all**, and §9.4 never actually claimed
it did beyond bundling the word "budgets" into the same sentence. The cold-start rule bounds how soon
*after a restart* the agent can make its *next* decision — a few minutes. It says nothing about how
many decisions it can make *in total across many restarts within a rolling 24h period*. An in-memory
counter reset to zero on every restart makes `max_changes_per_24h` trivially bypassable by restarting
the process — not a contrived attack, but an ordinary consequence of routine redeploys, crash loops,
or (in a future containerized deployment) pod evictions, none of which are rare events to design a
safety invariant around surviving.

PRD US-012 describes this exact field as "a hard cap on change frequency as a runaway guard," and its
own acceptance criteria call it a "rolling 24 h change budget... exhausting it logs a warning and
blocks further changes." A cap that a restart resets is not a hard cap. This is a safety-invariant
gap, not a persistence nice-to-have, and it exists in the design *before* any code implements it — the
right time to fix the requirement is now, not after M4 ships something that has to be revised.

## Decision

**The 24h change budget's per-disk change-timestamp record must survive a process restart. Cooldowns
remain in-memory-only, unchanged from the original v1 decision — that half of §9.4 was correct.**

Minimal scope, not a full state journal: what M4 (Decider + Guardrails, where budget enforcement
lives) needs is a durable, per-disk, append-only record of applied-change timestamps sufficient to
reconstruct "how many changes has this disk had in the trailing 24h" on startup, before any new
change is permitted. This is explicitly **not** the same commitment as the full "State/Journal"
component in the architecture diagram (last action, decision history, full audit trail) — that
remains an M5 (Observability) concern and stays exactly as deferred as before. Budget persistence is
pulled out of §9.4's deferred list into M4's own requirements; nothing else in §9.4 changes.

The concrete mechanism (a small local file, an embedded key-value store, appending to the eventual
decision journal early) is not decided here — M4 does not exist yet, and picking a specific
persistence technology speculatively would be exactly the kind of premature implementation this
project's own documentation-first discipline exists to avoid. What is decided is the requirement: M4
is not done until a restart cannot silently reset the 24h budget counter.

## Options Considered

### Option A: Leave both budget and cooldowns deferred as one bundled decision (status quo, rejected)

Rejected — this ADR exists because the bundled justification does not hold for the budget, and PRD
US-012 already calls it a hard cap. Leaving it as-is means shipping M4 with a documented gap in its
own stated safety guarantee.

### Option B: Persist everything now — budget, cooldowns, and decision history together

Rejected as over-scoped for what this specific gap needs. Cooldowns are provably safe to lose (once
the validation gap is closed) via the cold-start bound; persisting them anyway adds implementation
cost — a write on every cooldown start, not just every applied change — for a guarantee the design
already provides for free. Decision history persistence is a genuinely separate concern (observability
and auditability, not a safety invariant) that belongs with M5's decision journal, which will
naturally subsume this minimal budget record once it exists — this ADR does not preclude that, it
just doesn't require building the whole journal early to fix one specific gap.

### Option C: Persist only the budget counter, leave cooldowns in-memory (chosen)

Matches exactly what the safety argument requires and no more: the budget needs persistence because
nothing else bounds its loss; cooldowns don't, once validated symmetrically.

## Consequences

**Easier**
- `max_changes_per_24h` is an actual hard cap regardless of restart frequency, matching what US-012
  already promises.
- M4's scope for this is small — one durable per-disk timestamp record, not a general state store.

**Harder**
- M4 gains a persistence dependency (a local file or embedded store) it would not otherwise have had
  at that milestone; M5's decision journal was always going to need one, this just makes M4 need a
  minimal version of it slightly earlier.
- The now-symmetric cooldown validation rejects configs that previously passed
  (`cooldown.scale_down > scale_down_window`) — a stricter but correct rule; no shipped config in
  this repo's testdata or examples violates it.

**Revisit when**
- M5's decision journal is implemented — if it lands with the durability M4's minimal budget record
  needs, M4's version can be replaced by writing through the same store instead of maintaining two.

## Action Items

1. [x] `internal/config/validate.go`: reject `cooldown.scale_down > scale_down_window`, symmetric to
       the existing `cooldown.scale_up` check — this is what makes the in-memory-cooldown argument
       unconditionally true rather than accidentally true for the current default parameter set.
2. [x] M4 (Decider + Guardrails): the 24h budget counter must be reconstructible from durable storage
       on startup — no new change may be permitted before that reconstruction happens for the disk
       being evaluated.
3. [x] `docs/design.md` §9.4 corrected: budget persistence is no longer a deferred decision.

> **Refined by the M6 implementation (2026-08-29, issue #10 reopened):** "reconstructible on startup"
> turned out to be necessary but not sufficient once live actuation existed. Two gaps surfaced in the
> MVP review that this ADR's original text didn't anticipate. First, the original design recorded a
> change event only *after* `Actuator.Apply` returned successfully — a crash between the platform
> confirming the write and that record becoming durable silently undercounted a real, applied change
> against the hard cap, with nothing left on disk to reconstruct it from on the next restart. Second, a
> persistence failure mid-run (not at startup at all — e.g. the volume holding the state directory
> going away) was only logged; every subsequent write stayed enabled with no durable record backing it.
>
> `internal/state.Budget` is now a small intent/result state machine, not an append-only list of
> successes: `Reserve` durably records a change as `Pending` *before* the write is attempted (so the
> budget slot is consumed the instant the attempt begins, not after it's confirmed to have worked), and
> `Confirm`/`Release` resolve it afterward — `Pending` counts against the budget exactly like
> `Confirmed` does, since undercounting is the unsafe direction and only a conclusively-verified
> non-write (`Released`) should ever stop counting. A `Pending` reservation a previous run never
> resolved survives a restart and is reconciled once at startup, right after the first successful
> discovery, against a fresh live read of the disk — never against this process's own possibly-stale
> `Decision.Current` snapshot. Any persistence failure, at any point, marks `state.Tracker` permanently
> unhealthy for the rest of the process's life (`Tracker.State` then reports the budget as exhausted via
> the existing `budget_exhausted` guardrail path, and `Agent.actuate` refuses to even attempt a further
> reservation), closing the mid-run gap the same way. `LoadBudget` also now fails closed on corruption
> (previously a corrupt line anywhere was silently skipped, which can only ever understate the true
> count) while still recovering cleanly from a torn tail write, the one shape of corruption a crash
> mid-append can actually produce (the fsync in `append()` guarantees every line before the last one is
> already durable). See `docs/design.md` §9.4's "Budget crash-safety" note and
> `internal/state/budget.go`'s own doc comments for the current design.

4. [x] `internal/state/budget.go`: `Reserve`/`Confirm`/`Release`/`Pending` intent/result log;
       `LoadBudget` fails closed on mid-file corruption, recovers a torn tail write; `Compact` never
       drops a still-pending reservation regardless of age; both writes and the compacted rewrite fsync
       their containing directory.
5. [x] `internal/state/tracker.go`: `Tracker.Healthy()`/sticky `budgetHealthy`; `State()` reports the
       budget exhausted once unhealthy.
6. [x] `cmd/mws-iops-autoscaler/agent.go`: `actuate()` reserves before the write and
       confirms/releases after; a new `reconcilePendingBudget` resolves leftover reservations once at
       startup; `Tracker.Healthy()` checked before ever reaching the Actuator.
7. [x] `internal/health`/`internal/telemetry`: `budget_healthy` exposed on both `/readyz` and
       `/metrics`.
8. [x] Tests: reload with a pending reservation, corruption in the middle (rejected) vs. a torn tail
       (recovered), compaction never dropping a pending entry, a Reserve/Confirm failure flipping
       `Healthy()` false and forcing `budget_exhausted`, and Agent-level reconciliation for all four
       outcomes (confirmed, released, ambiguous-stays-pending, disk-no-longer-managed-stays-pending).

> **Refined again (2026-08-29, pre-merge review): a write's own error is not proof it never landed.**
> `actuate()`'s error branch released the reservation unconditionally — the comment reasoning at the
> time claimed this was "safe without a fresh read: an actuator error means Apply's own pre-write
> checks already refused before calling the platform, or the platform call itself failed outright —
> neither leaves live state at `d.Target`." That reasoning is false for the one case it matters most:
> the actual mutating call (`UpdateDisk`/`UpdateVirtualMachine`) can fail on the client side — timeout,
> connection reset, a lost response — *after* the platform has already applied it server-side. Releasing
> in that case violates `Budget.Release`'s own documented precondition (live state verified to still
> match `Before`) and silently undercounts a real, applied change against the hard cap — reopening
> exactly the gap `Reserve`-before-write exists to close, just moved from "process crash" to "any failed
> write call."
>
> `Agent.resolveReservationAfterWriteFailure` now applies startup reconciliation's own three-way
> resolution mid-run, immediately after any non-dry-run write error, instead of trusting the error:
> live current equal to `Target` confirms the reservation and adopts the value into `a.managed`/
> `disk_provisioned_iops` exactly as a verified success would (the next cycle must decide from reality,
> not a stale `Current`); equal to `Before` releases it — the one outcome where `Release`'s contract is
> actually satisfied; anything else, including a failed live read itself, leaves it pending until the
> next restart's reconciliation, the same fail-closed default `reconcilePendingBudget`'s own `default:`
> case already uses. A dry run is deliberately exempt and still releases unconditionally on error: an
> independent disk's dry run goes through the platform's own `validateOnly`, which by contract never
> mutates regardless of whether the response is lost, and a dependent disk's dry run returns before
> `UpdateDependentDisk` is ever called at all (ADR-007 — no server-side dry run exists for that path) —
> neither can produce the ambiguity this refinement exists to close.

9. [x] `cmd/mws-iops-autoscaler/agent.go`: `resolveReservationAfterWriteFailure` resolves a real write's
       reservation from a fresh live read after any error, reusing `liveCurrentIOPS`; dry-run errors
       still release unconditionally.
10. [x] Tests: a failed write whose live state matches `Before` (released), matches `Target` (confirmed
        and adopted despite the error), matches neither (stays pending), and a failed live-state read
        itself (stays pending) — plus a dedicated dry-run-error-always-releases test.
