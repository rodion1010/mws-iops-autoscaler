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
2. [ ] M4 (Decider + Guardrails): the 24h budget counter must be reconstructible from durable storage
       on startup — no new change may be permitted before that reconstruction happens for the disk
       being evaluated.
3. [x] `docs/design.md` §9.4 corrected: budget persistence is no longer a deferred decision.
