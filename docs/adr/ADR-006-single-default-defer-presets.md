# ADR-006: Phase 1 ships one built-in default parameter set; named presets deferred

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Supersedes:** the three-preset model (`balanced` / `performance` / `cost`) in
`docs/configuration.md` §3 as a Phase 1 deliverable; amends ADR-002's "On exposure" paragraph and
action item 7

## Context

`docs/configuration.md` §3 documented three built-in presets, each a full `advanced` parameter set,
selected in config via `policy.preset: balanced | performance | cost`. ADR-002 had settled on presets
as the intended shape of the user-facing surface: "Users pick a preset... and the underlying values
can be recalibrated without breaking anyone."

Implementing the config package for Phase 1 (M0: schema, resolver, `--validate-config`) required
writing the §8 cross-field validation rule `cooldown.scale_up > scale_up_window` ("the agent could
never act") as actual code, then testing it against the documented presets to catch drift early. The
`performance` preset failed its own rule:

```
performance: {evaluation: {scale_up_window: 2m, ...}, cooldown: {scale_up: 3m, ...}}
```

`cooldown.scale_up` (3m) exceeds `scale_up_window` (2m) — this preset could never actually trigger a
second scale-up, which is exactly the failure mode the rule exists to catch. The bug was in the
documented preset table itself, not in a user's config, so `--validate-config` alone could not have
caught it before this: it validates what a user writes, not the three tables shipped as options.

## Decision

**Phase 1 ships the `balanced` preset's values as the single built-in default parameter set.** There
is no `preset` field in the schema. An operator overrides individual fields through `policy.advanced`
(opt-in) or `policy.limits`, not by choosing a name.

`performance` and `cost` are **deferred, not deleted** — their draft values are kept in this ADR for
whenever named presets are reintroduced (candidate for Phase 5, PRD §6). `performance`'s numbers are
recorded here already corrected, not as they shipped in the first draft:

```yaml
performance:                           # earlier up, slower down, more headroom
  evaluation:  {scale_up_window: 3m,  scale_down_window: 60m}   # was 2m — violated cooldown.scale_up
  saturation:  {utilization_threshold: 0.95, queue_depth_threshold: 1,
                latency: {mode: relative, baseline_multiplier: 1.25}}
  target:      {scale_up_utilization: 0.75, scale_down_utilization: 0.60}
  cooldown:    {scale_up: 3m,  scale_down: 60m}
  step:        {min_change_iops: 200, max_scale_up_ratio: 2.0, max_scale_down_ratio: 0.75}

cost:                                   # later up, tighter fit, faster down
  evaluation:  {scale_up_window: 10m, scale_down_window: 20m}
  saturation:  {utilization_threshold: 1.00, queue_depth_threshold: 2,
                latency: {mode: relative, baseline_multiplier: 2.0}}
  target:      {scale_up_utilization: 0.90, scale_down_utilization: 0.80}
  cooldown:    {scale_up: 10m, scale_down: 20m}
  step:        {min_change_iops: 300, max_scale_up_ratio: 1.5, max_scale_down_ratio: 0.5}
```

`cost` already satisfied every §8 rule as originally drafted and needed no correction.

> **Amended 2026-08-29.** The internal registry is laid out now, not deferred to Phase 5: `internal/config`
> keeps a `PresetName` type and a `presets map[PresetName]ResolvedAdvanced` with all three entries
> (`balanced`, `performance` corrected, `cost`), each covered by a test asserting it satisfies §8's
> own validation — the exact check that would have caught the original bug before it shipped. What
> stays deferred to Phase 5 is *exposure*: no `preset` field on `PolicySpec`, nothing in the config
> schema or `--validate-config` lets an operator select `performance` or `cost` today; only
> `presets[DefaultPresetName]` (`balanced`) is ever read. Phase 5 adds the field, the resolver's
> preset-rebase-on-override logic, and an unknown-preset validation rule against a registry that
> already exists and is already known-consistent — it does not re-derive the numbers.

## Options Considered

### Option A: Patch `performance` and keep three presets

Change `scale_up_window` to `3m` (or `cooldown.scale_up` down to `2m`) and ship all three as
originally planned. Rejected for Phase 1: it fixes today's rule, not the underlying risk — three
tables now have to stay mutually consistent under every validation rule this project adds later
(scale-down calibration, a future VM-cap-aware bound, whatever ADR-002's "Revisit when" items bring).
One default set has that problem once, not three times, while there is no evidence yet that any
operator needs the choice.

### Option B: Chosen — one default, presets deferred

Removes the field entirely rather than shipping it with one legal value (`balanced`) — a `preset`
field that only accepts one value is a field with no purpose, and keeping it would misrepresent the
MVP as offering a choice it doesn't. `performance` and `cost` stay documented, corrected, and ready to
reintroduce once there is a real reason to choose between tunings (ADR-002's calibration data, or a
specific operator request).

### Option C: Drop the preset concept permanently, redesign around raw knobs

Rejected: contradicts ADR-002's "On exposure" reasoning, which still holds — a config surface of nine
raw thresholds is worse than a name plus targeted overrides once there is more than one tuning worth
naming. This ADR defers the *feature*, not the *design principle*.

## Consequences

**Easier**
- ~~One `ResolvedAdvanced` value instead of a map keyed by preset name~~ — superseded by the
  amendment above: the registry exists now, so this benefit was never realized. What Phase 1 keeps is
  narrower but real: `--validate-config` cannot reject a config for naming an unknown preset, because
  nothing in the schema lets a config name one at all.
- The resolver (`resolve.go`) has no preset-selection branch to test or keep correct yet — `Resolve`
  reads `defaultAdvanced` directly, unconditionally.

**Harder**
- No user-facing choice between "react fast, pay more" and "react slow, pay less" until presets
  return. An operator who wants that today writes `policy.advanced` overrides by hand.
- Reintroducing presets later is still work — a `preset` field, `--validate-config` rejecting an
  unknown one, and the resolver's preset-rebase-on-override logic from the first draft — but the
  registry itself is not part of that work anymore per the amendment above. Additive either way: no
  Phase 1 config becomes invalid when it happens.

**Revisit when**
- ADR-002's "Revisit when" telemetry arrives and shows the single default is measurably wrong for a
  meaningful slice of workloads — the original motivation for `performance` / `cost` in the first
  place.
- An operator explicitly asks for the performance/cost tradeoff rather than hand-writing `advanced`
  overrides.
