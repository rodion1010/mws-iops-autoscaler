# ADR-014: Fail closed on stale metrics and stale discovery snapshots

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Closes:** GitHub issue #15 (P0)

## Context

`WindowAnalyzer.Analyze` decides `WindowFullUp`/`WindowFullDown` (ADR-010) purely from the timestamp
*span* of the buffered sample slice: `newest.Timestamp - oldest.Timestamp >= window`. That span is a
property of the samples the Sampler happened to collect before sampling stopped — it says nothing
about whether sampling is still happening *now*. A disk whose guest source silently breaks (a crashed
collection goroutine, a permissions change on `/proc/diskstats`, a disk unplugged from the VM) keeps
whatever samples it last collected sitting in the ring buffer. Every timestamp-span check on that
buffer keeps returning exactly the same answer it did the moment sampling stopped — including
`Saturated`, if the buffer happened to freeze mid-saturation. Nothing inside a fixed buffer can ever
reveal that time has kept passing outside it.

The same shape of problem exists one layer up, in `cmd/mws-iops-autoscaler/agent.go`. `discover()`
populates `a.managed` from a live Compute API call, but reconcile always operates on whatever
`a.managed` currently holds, not on the discovery call itself. If `discover()` starts failing (Compute
API outage, expired credentials), `readiness.SetDiscoverySucceeded(false)` correctly flips `/readyz`
to not-ready — but reconcile keeps running against the last known-good snapshot indefinitely, with no
bound on how old it's allowed to get before that snapshot's `Current` IOPS values or its very list of
managed disks might no longer be accurate.

A third, narrower bug was found alongside these while auditing the same code path: `discover()` set
`readiness.SetSourceReachable(true)` immediately after a successful Compute API call, on the reasoning
that "local mode's guest source needs no network, so proving the Compute API reachable is enough."
That reasoning conflates two independent dependencies — the guest source reads `/proc/diskstats`
locally, which can fail for reasons that have nothing to do with Compute API reachability (permissions,
a missing disk file). A metrics-collection outage with a healthy Compute API connection never showed
up in `/readyz` at all.

## Decision

**Evidence that cannot be proven current must never drive a scale-up or scale-down decision.** Three
independent freshness checks, one per layer:

1. **Sample freshness (`Analysis.Fresh`).** `Analyze` gains an explicit `now time.Time` parameter
   (mirroring `Decider.Decide`'s own, and for the identical reason: freshness is a comparison against
   real wall-clock time passing, which no amount of clever computation over the sample slice alone can
   substitute for). `Fresh = now.Sub(newest.Timestamp) <= p.MaxSampleAge`. Both `Saturated` and
   `Underutilized` gain `Fresh &&` as their first AND-term — stale evidence makes every verdict
   `false`, not just suspect.

2. **Gap-aware window coverage.** `windowFull` (ADR-010) is extended to also reject a window if any two
   *consecutive* samples inside it are farther apart than `MaxSampleGap`, not just check overall span.
   A window whose span covers `scale_up_window` but contains one large silent gap (a missed collection
   cycle, a source outage that later recovered) previously read as "full" even though most of it
   carries no real information. This closes the gap-detection half of reopened issue #7's "buffer
   slack + gap-aware window coverage."

3. **Discovery freshness (`Agent.maxDiscoveryAge`).** `reconcile()` now checks
   `now.Sub(lastDiscoverySuccess) > maxDiscoveryAge` before analyzing a single disk, and skips the
   entire cycle if so (logged, not silently). A zero-value `lastDiscoverySuccess` (discovery has never
   once succeeded) is treated as maximally stale, not as "no opinion, proceed anyway" — the fail-closed
   default has to hold at start-up too, not just after a prior success expires.

Both `MaxSampleAge`/`MaxSampleGap` (`EvaluationPolicy`) and `maxDiscoveryAge` (`Agent`) are derived by
`main.go` from `runtime.collection_interval`/`runtime.discovery_interval` — 3x, 2x, and 3x
respectively — rather than being separate user-facing config fields. A threshold that isn't a multiple
of the cadence it bounds would either trip on ordinary jitter (too tight) or defeat its own purpose
(too loose); tying it to the interval that's already the single canonical cadence (ADR-010) keeps there
being one thing to tune, not two that can drift out of sync. Calibration-pending, same treatment as
every other implementation constant of this shape (design.md §8.5).

`Decider.Decide`'s default `NoAction` branch is split into three distinct reasons instead of one
generic `no_signal`, so the journal and metrics can tell these apart:

| Reason | Condition |
|---|---|
| `metrics_stale` | `!a.Fresh` |
| `window_not_full` | `a.Fresh` but neither `WindowFullUp` nor `WindowFullDown` |
| `no_signal` | `a.Fresh` and at least one window full, nothing triggered |

`readiness.SetSourceReachable` is decoupled from `discover()`'s Compute API success and driven solely
by `sampler.Tick()`'s own return value, at every call site (the initial tick in `Run()` and the
ticker-driven tick in the main loop) — fixing the conflated-dependency bug found alongside the main
freshness gap.

## Options Considered

### Option A: Leave freshness undetected, rely on operators noticing (status quo, rejected)

Rejected — this is exactly the "silent failure that looks like healthy quiet" class of bug the PRD's
own NFR-9 ("no action without a full, sustained observation window") already exists to prevent for the
cold-start case; a stale-buffer failure is the same risk in a different guise, and silent by
construction (nothing else changes when sampling stops).

### Option B: Detect staleness only at the Agent/discovery layer, not inside Analyzer

Rejected — this would catch a total metrics-source outage (the Sampler itself erroring) but not a
partial one where `sampler.Tick()` keeps returning success while quietly failing to update specific
disks' buffers, or a bug in the collection goroutine that stops appending samples without surfacing an
error. `Analysis.Fresh` is the layer that's actually authoritative about "is *this disk's* data
current," independent of whatever the Sampler's own error-reporting does or doesn't catch.

### Option C: Both layers (chosen)

Sample-level freshness catches per-disk staleness even when the Sampler reports success; discovery-
level freshness catches the case where the whole managed-disk snapshot itself may be wrong. Neither
subsumes the other.

## Consequences

**Easier**
- A metrics or discovery outage now reads unambiguously in the journal/metrics as
  `metrics_stale`/discovery-skip rather than silently continuing to report whatever verdict was true
  the moment the outage began.
- `/readyz`'s `source_reachable` now means what it says for both `local` and (future) `fleet` modes.

**Harder**
- Three more constants to reason about (`MaxSampleAge`, `MaxSampleGap`, `maxDiscoveryAge`), all
  calibration-pending like every other implementation constant already in this codebase.
- A disk that's genuinely fine but whose collection cycle briefly stutters past `MaxSampleAge` now
  reports `metrics_stale` for one cycle rather than continuing to act on marginally-old data — an
  intentional trade of a little responsiveness for the fail-closed guarantee.

**Revisit when**
- The `--dry-run` calibration harness (§8.5) has real-workload data to tune the 3x/2x/3x multipliers
  against, the same way every other threshold in this codebase is meant to be calibrated.

## Action Items

1. [x] `internal/analyzer/types.go`: `EvaluationPolicy.MaxSampleAge`/`MaxSampleGap`;
       `Analysis.Fresh`/`SampleAge`; `Analyzer.Analyze` gains `now time.Time`.
2. [x] `internal/analyzer/analyzer.go`: `Fresh`/`SampleAge` computation; `Fresh &&` prepended to
       `Saturated`/`Underutilized`; `windowFull` gap-checking.
3. [x] `internal/decider/decider.go`: default branch split into `metrics_stale`/`window_not_full`/
       `no_signal`.
4. [x] `cmd/mws-iops-autoscaler/agent.go`: `maxDiscoveryAge`/`lastDiscoverySuccess`; reconcile's
       staleness guard; `SourceReachable` driven by `sampler.Tick()` instead of `discover()`;
       `telemetry.RecordStaleness` wired from `Analysis.SampleAge`.
5. [x] `cmd/mws-iops-autoscaler/main.go`: derive and thread the three thresholds from
       `runtime.collection_interval`/`runtime.discovery_interval`.
6. [x] `docs/design.md` synced: `Analysis`/`Analyzer`/`Decider` contracts, §6.1 failure-mode table,
       §7.3 readiness semantics.
