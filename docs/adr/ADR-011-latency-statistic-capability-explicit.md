# ADR-011: Latency statistics carry their own derivation, never forced uniform across sources

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Amends:** the `Sample` doc comment and §4.4/§4.5's `p95_latency`/`baseline_p95` treatment in
`docs/design.md` — no shipped code changes (the Analyzer this affects is M3, not yet built)

## Context

`Sample` (M2, shipped) deliberately carries only an average latency per interval —
`ReadLatAvg`/`WriteLatAvg`, `time.Duration`, "average latency this interval, not a percentile." That
much is correct and unaffected by this ADR: `/proc/diskstats` genuinely exposes only cumulative
ticks, so an interval average is all `guest` can ever honestly produce at the `Sample` level.

Where `Sample`'s own doc comment went further was in describing the not-yet-built Analyzer's plan for
turning these into the p95 that §4.4's saturation formula and §4.5's demand estimator both consume:
"the Analyzer computes the actual p95 from a window of these per-interval averages, **uniformly for
both sources**." The stated reason was to avoid a repeat of the `QueueDepth` unit-mixing bug — if
`platform` computed its p95 differently from `guest`, the same field name would mean two different
statistical quantities depending on `Source`, an invisible footgun.

That reasoning correctly identifies a real risk but draws the wrong conclusion from it. `platform`
is not just "another average-producing source" — docs/platform-reference.md's measured Prometheus export confirms
`compute_vm_disk_read_latency_hist_bucket`/`_count`/`_sum` are a real histogram, not an average: a
platform-side p95 computed directly from those buckets for a single scrape is a materially more
accurate statistic than a p95 estimated by windowing a series of per-interval averages, which is a
coarser approximation on both sources but is *all* `guest` can ever produce. Forcing `platform` down
to the same windowed-average method — the fix actually implemented by "uniformly for both sources" —
discards real accuracy the platform metric already provides, for a source that PRD US-005b's
comparison mode exists specifically to validate before `fleet` mode has to depend on it with no guest
fallback at all (ADR-001). Validating "does the platform's real, histogram-derived latency estimate
track what guest observes directly" is a more useful question than validating "does the platform's
deliberately-dumbed-down average-based estimate track guest" — the latter proves the two sources can
agree on a computation neither of them is being asked to actually use in `fleet` mode, where no
histogram-vs-average choice will exist to fall back on.

The `QueueDepth` precedent this comment invokes did not solve its own problem by picking the lower-
fidelity source's method for everyone — it solved it by carrying the unit explicitly on the value, so
`ops` and `bytes` can never be silently compared but neither source loses what it can actually report.
The same move applies here and was available the whole time.

## Decision

**A latency statistic carries its own derivation alongside its value**, the same move `QueueDepth`
already makes for units:

```go
// LatencyStatKind names how a latency statistic was derived. Never
// compared or thresholded across differing kinds without accounting for
// the difference — the same problem QueueDepth's explicit unit solves,
// applied to latency instead of queue depth.
type LatencyStatKind string

const (
	// LatencyWindowedAvg: a percentile estimated from a window of
	// per-interval averages (Sample.ReadLatAvg/WriteLatAvg). The only
	// method `guest` can ever produce — /proc/diskstats exposes no
	// within-interval distribution, only cumulative ticks. `platform`
	// falls back to this when no histogram is available.
	LatencyWindowedAvg LatencyStatKind = "windowed_avg"
	// LatencyHistogram: a real quantile computed directly from the
	// platform's exported bucket histogram
	// (compute_vm_disk_{read,write}_latency_hist_*, docs/platform-reference.md) for a
	// single scrape — no windowing needed for accuracy, though the
	// Analyzer may still window it for sustain-over-time checks.
	// `platform` only; `guest` has no histogram to read.
	LatencyHistogram LatencyStatKind = "histogram"
)

type LatencyStat struct {
	P95  time.Duration
	Kind LatencyStatKind
}
```

`Analysis` (referenced by `Decision.Inputs` in §5.2 but never itself defined until this ADR) carries
`ReadLatency`, `WriteLatency`, and `Baseline` as `LatencyStat`, not bare `time.Duration` — so which
method backed a given saturation verdict is part of the evidence in the decision journal, answering
"why didn't it scale?" (§4.3) with the same precision already required of every other input.

`Sample` itself is unchanged — `ReadLatAvg`/`WriteLatAvg` remain the correct, source-agnostic raw
ingredient regardless of which method the Analyzer later derives a percentile with.

## Options Considered

### Option A: Uniform windowed-average p95 for both sources (the previous plan, rejected)

**Pros**
- One computation path; `Analysis.ReadLatency` always means the same thing regardless of `Source`.

**Cons**
- Throws away real accuracy `platform` already provides, for the source PRD US-005b's shadow mode
  exists to validate before `fleet` has to trust it alone.
- "Uniform" here means "as coarse as the least capable source can manage," not "correct for both."

### Option B: Each source computes its own best p95, `LatencyStat` records which method (chosen)

**Pros**
- `guest` and `platform` each report the most accurate statistic they can actually produce.
- The `QueueDepth` precedent already established this pattern is how this codebase handles a
  source-dependent statistic — applying it to latency is consistency, not a new idea.
- Comparison mode (US-005b) validates the statistic `fleet` will actually depend on, not a
  deliberately weakened stand-in.

**Cons**
- The Analyzer needs two code paths for latency instead of one. Mitigated: `LatencyWindowedAvg` is
  required regardless (guest has no alternative, and platform needs a fallback for when the
  histogram metrics are absent or a scrape is stale), so this is the one path plus an additional,
  better path for `platform` when available — not a second unrelated implementation.

### Option C: Expose only the platform's histogram-derived value; drop guest's windowed average entirely

Rejected — `guest` (`local` mode) is the primary source (ADR-001) specifically because it is
available with no monitoring role and no ~135 s platform detection floor. It has no histogram to
read; requiring one would mean `local` mode could never compute a percentile at all.

## Consequences

**Easier**
- `platform`'s real accuracy is available to the Analyzer and, eventually, to `fleet` mode, instead
  of being designed away before it is ever used.
- The decision journal can answer not just *what* the latency figure was but *how confidently* it was
  known — a windowed-average estimate and a histogram-derived quantile warrant different trust when
  reviewing a "why did it scale" question.

**Harder**
- None beyond Option B's already-mitigated con — this is a documentation and future-type-model
  change; no shipped code exists yet to modify (Analyzer is M3).

**Revisit when**
- M3 (Analyzer) is implemented — `LatencyStatKind`/`LatencyStat`/`Analysis` land in
  `internal/metrics` or a new `internal/analyzer` package at that point, not before.
- A platform `PlatformSource` (Phase 2) is implemented and its histogram-to-quantile interpolation
  method needs to be chosen and documented (linear interpolation within the bucket the target
  quantile falls in is the standard Prometheus approach; not decided here since no code exists yet
  to make that call concrete).

## Action Items

1. [ ] When the Analyzer (M3) is implemented, define `LatencyStatKind`/`LatencyStat` and give
       `Analysis` `ReadLatency`/`WriteLatency`/`Baseline LatencyStat` fields, per the Decision above.
2. [ ] When `PlatformSource` (Phase 2) is implemented, compute `LatencyHistogram` from the exported
       bucket histogram when the metrics are present and fresh; fall back to `LatencyWindowedAvg`
       otherwise, following the same present-and-non-zero fallback pattern US-005a already
       establishes for utilization and queue depth.
3. [x] `docs/design.md` §4.4/§4.5/§5.2 corrected to reflect this; `internal/metrics.Sample`'s doc
       comment corrected to stop describing the superseded "uniform for both sources" plan.
