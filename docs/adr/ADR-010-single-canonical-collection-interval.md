# ADR-010: One canonical sampling cadence; window readiness is timestamp-based

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Amends:** the schema shape described in `docs/configuration.md` §2/§3 (an example, not yet a
shipped field — no released config has used `evaluation.collection_interval`)

## Context

The config schema carried `collection_interval` in two places: `policy.advanced.evaluation`
(inheritable per VM or per disk) and `runtime.metrics` (one process-wide value). `Resolve` kept both
as independent values — nothing reconciled them, and nothing could, because the architecture only
supports one.

`internal/metrics.Sampler` (M2, already built) is constructed once per process and ticks a single
`MetricSource.Sample(ctx, disks []DiskRef)` call that returns every managed disk's sample for that
tick in one round — `local` mode reads `/proc/diskstats` once, `fleet` mode makes one Prometheus
export call per project (design.md §4.2: "`fleet` sampling is per-project, not per-disk: one HTTP
call yields every disk in the project"). There is no per-disk timer and no path to add one without a
different collection architecture entirely — sampling one disk on its own schedule would mean either
one goroutine and one HTTP/file read per disk (defeats the whole point of a project-wide export call)
or accepting that a "per-disk interval" is actually just a per-disk filter applied to samples still
collected on the one real cadence, which is a different feature wearing the same field name.

So a `policy.advanced.evaluation.collection_interval` override was never implementable as written —
setting it on a disk would silently do nothing, since nothing downstream reads that value on a
per-disk basis. That's a worse failure mode than not offering the field at all: a config that looks
like it does something and doesn't.

Separately, `internal/metrics.NewSampler(source, capacity)` sizes its ring buffer by a raw sample
*count* ("capacity is the caller's responsibility to size — typically the longest configured window
divided by the collection interval"), and the not-yet-built Analyzer's `window_not_full(disk)` check
(design.md §4.3) was implicitly going to be count-based too. A count-based readiness check is
correct only as long as every sample was actually collected at the assumed interval — a missed tick,
a slow scrape, or (had the per-policy field been honored) a genuinely different cadence per disk would
silently under- or over-count how much real time a "full" window represents. `Sample.Timestamp`
already carries the metric's own timestamp (design.md §5.2: "metric timestamp, not wall clock"), so
the information needed for a correct check already exists and was simply unused for this purpose.

## Decision

**`runtime.metrics.collection_interval` is the only sampling-cadence field.**
`policy.advanced.evaluation.collection_interval` is removed from the schema — not deprecated, not
defaulted, removed — so setting it is rejected by strict decoding as an unknown key, the same
treatment every other superseded field gets (`device:`/`disk_id:`, ADR-004).
`policy.advanced.evaluation` keeps `scale_up_window` and `scale_down_window`, which are genuinely
per-policy: how much history a decision needs, not how often it's collected.

**Window readiness is computed from `Sample.Timestamp` span, not a sample count.** "Window full" for
a given check means the buffered samples for that disk span at least the configured window duration,
measured `newest.Timestamp - oldest.Timestamp`, not `len(samples) >= window / collection_interval`.
This makes readiness correct regardless of tick jitter, missed samples, or scrape latency, and removes
the last reason a per-policy cadence might have seemed necessary.

## Options Considered

### Option A: Keep both fields, document that the policy-level one wins

Rejected. There is no mechanism by which it *could* win — the Sampler has no per-disk schedule to
change. Documenting a field as authoritative when nothing reads it that way is worse than removing it.

### Option B: Make the Sampler actually support per-disk cadence

Rejected for Phase 1. It would mean either abandoning the one-call-per-project efficiency `fleet`
sampling depends on, or building a per-disk filter that *pretends* to be a different cadence while
still collecting on the real one — which is Option A's problem again, just moved into the Sampler
instead of the config layer. Nothing in the PRD (FR-13's tunable list has no per-disk collection
interval) or the observed platform behavior (30 s publication interval is fixed and shared across a
whole project) motivates this cost. Revisit only if a future disk genuinely needs a materially
different scrape cadence than its siblings — not requested anywhere today.

### Option C: One field, count-based window readiness (chosen field, rejected readiness model)

Removing the duplicate field alone would fix the "two sources of truth" problem but leave the
count-based `window_not_full` check silently wrong whenever real cadence doesn't match the configured
one — which becomes more likely, not less, once there is exactly one authoritative interval that
every disk is assumed to hit on the dot. Timestamp-based readiness costs nothing (the field already
exists on `Sample`) and removes an entire class of "window said full, wasn't really" bugs.

### Option D: One field, timestamp-based readiness (chosen)

## Consequences

**Easier**
- One config field to reason about (`runtime.metrics.collection_interval`), matching what the Sampler
  actually does.
- `window_not_full` is correct under tick jitter, missed samples, or a future scrape source with its
  own irregular cadence, with no extra bookkeeping.
- Removes a field that would have silently done nothing if set — a class of config bug design can
  eliminate for free before it ships.

**Harder**
- None identified: no code outside this package read the removed field (grepped before removing it),
  and the not-yet-built Analyzer picks up the timestamp-based rule as its original design instead of
  needing a later correction.

**Revisit when**
- A real requirement for per-disk sampling cadence appears (Option B) — with a concrete reason, not a
  config field carried over from an earlier draft.

## Action Items

1. [x] Remove `EvaluationSpec.CollectionInterval` (`internal/config/types.go`).
2. [x] Remove `ResolvedAdvanced.CollectionInterval` and its resolution in `applyAdvancedOverride`
       (`internal/config/defaults.go`, `resolve.go`).
3. [x] Confirm strict decoding rejects `evaluation.collection_interval` as an unknown key
       (`internal/config/load_test.go`).
4. [x] When the Analyzer (M3) implements `window_not_full`, compute it from `Sample.Timestamp` span
       against `policy.advanced.evaluation.scale_up_window` / `scale_down_window` — not a count.

> **Refined by the M3/M6 implementation (2026-08-29, issue #7 reopened):** two gaps surfaced in the
> MVP review that this ADR's text didn't anticipate. First, "compute from timestamp span" turned out
> to need feeding `windowFull` the already-sliced trailing window (`trailingWindow(samples, window)`),
> not the whole retained buffer — calling it on the full buffer let the minimum-sample-count condition
> be satisfied by samples outside the window actually being evaluated. Second, span-based readiness
> alone doesn't reject a large *internal* gap (a missed collection cycle, a temporary outage that later
> recovered) sitting inside an otherwise span-satisfying window — `windowFull` now also checks that no
> two consecutive samples inside the window are farther apart than a new `EvaluationPolicy.MaxSampleGap`.
> Separately, the ring-buffer capacity formula this ADR's own text estimates as "the longest configured
> window divided by the collection interval" (§context, `NewSampler`'s own doc comment) has an
> off-by-one: N samples at exact cadence span only `(N-1) × interval` end to end, so a
> `floor(window/interval)`-sized buffer is always one sample short of ever spanning the configured
> window — with the default 30-minute scale-down window and a 1-minute cadence, a capacity-30 buffer
> could span at most 29 minutes, permanently blocking `WindowFullDown`. `config.ResolvedConfig.SamplerCapacity()`
> now computes `ceil(window/interval) + 1 + slack` instead. See `internal/analyzer/analyzer.go`'s
> `windowFull`/`trailingWindow`, `internal/config/resolve.go`'s `SamplerCapacity`, and ADR-014 (the
> related `now`-driven staleness work from the same review pass) for the current state.

> **Refined again (2026-08-29, pre-merge review): the ring-buffer off-by-one had a sibling in
> `trailingWindow` itself.** `trailingWindow` trimmed every sample strictly before `cutoff =
> newest.Timestamp - window`, keeping only samples with `Timestamp >= cutoff`. That trim's own returned
> span (`newest.Timestamp - samples[0].Timestamp`) is mathematically bounded by `window`, never above
> it — and cutoff, computed from real collection timestamps, essentially never lands exactly on an
> existing sample (ordinary scheduler jitter, not a fault). So the first sample a strict trim keeps is
> almost always some small delta *after* cutoff, making the trimmed span some small delta *under*
> `window`, forever. `windowFull`'s own `span >= window` check — the entire reason `trailingWindow` gets
> called ahead of it — could then essentially never pass on real, jittered data, the same "buffer that
> can never fill" deadlock this ADR's first refinement fixed for the ring-buffer capacity formula, one
> layer downstream of it.
>
> `trailingWindow` now reaches one sample further back than the strict trim — but only when the
> strictly-trimmed span itself already falls short of `window`; if the natural trim already spans a full
> window on its own, nothing needs bridging, and reaching back anyway would risk pulling in a
> predecessor from the far side of an arbitrarily large gap (stale pre-outage history, exactly what the
> refinement directly above this one exists to keep out) for no benefit. When a predecessor genuinely is
> needed, `windowFull`'s existing `MaxSampleGap` check remains an entirely independent safeguard: a
> predecessor that turns out to sit across a real discontinuity, not ordinary one-interval jitter, still
> fails that check exactly as it always did.

## Action Items (continued)

5. [x] `internal/analyzer/analyzer.go`: `trailingWindow` reaches one sample past a strict cutoff trim,
       conditioned on the natural trim's own span falling short of `window`.
6. [x] Tests: a jittered-but-continuous 31-sample series proves `trailingWindow`'s returned span always
       reaches `window` and `windowFull` reads it as full; a clean post-outage recovery segment whose
       natural span already reaches `window` proves the stale pre-outage predecessor is never pulled in.
