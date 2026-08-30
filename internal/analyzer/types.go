// Package analyzer turns a disk's buffered metric samples into saturation
// evidence and verdict — windows, percentiles, latency baselines,
// saturated/underutilized (design.md §4.4/§4.5, M3). It is pure over a
// sample slice plus policy: no network, no state beyond the per-disk
// latency baseline it must remember across calls (ADR-012's "establish
// once, freeze while saturated" needs memory the input samples alone don't
// carry). Producing a target from this evidence is the Decider's job (M4,
// design.md §3.2) — kept separate so both stay independently testable.
package analyzer

import (
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

// EvaluationPolicy is the narrow slice of policy.advanced the Analyzer
// needs. It is a plain struct, not internal/config.ResolvedAdvanced, so
// this package stays decoupled from internal/config the same way
// internal/metrics stays decoupled from internal/compute (design.md §3.3)
// — translating a resolved policy into this shape is the orchestrator's
// job, not this package's. LatencyMode is deliberately absent: v1 is
// baseline-relative only ("Absolute latency thresholds" is out of scope,
// design.md §2/§9.4).
type EvaluationPolicy struct {
	ScaleUpWindow             time.Duration
	ScaleDownWindow           time.Duration
	UtilizationThreshold      float64
	QueueDepthThreshold       float64
	LatencyBaselineMultiplier float64

	// MaxSampleAge bounds how old the newest buffered sample may be before
	// Analysis.Fresh goes false (issue #15). Sampling that has silently
	// stopped leaves a buffer that is internally consistent forever — span
	// and count alone can never reveal that, only a comparison against wall
	// clock can (see Analyze's own doc comment). Calibration-pending, same
	// as the constants in analyzer.go; main.go derives it from
	// runtime.collection_interval rather than hardcoding it here.
	MaxSampleAge time.Duration

	// MaxSampleGap bounds the largest allowed gap between two consecutive
	// samples inside a window before that window stops counting as full
	// (issue #15, issue #7's "gap-aware window coverage"). Without this, a
	// window whose span happens to cover ScaleUpWindow/ScaleDownWindow but
	// contains one enormous silent gap (a missed collection cycle, a
	// temporary source outage that later recovers) reads as "full" even
	// though most of it carries no real information.
	MaxSampleGap time.Duration
}

// LatencyStatKind names how a latency percentile was derived — carried on
// the value itself so a windowed estimate and a (future) histogram-derived
// quantile are never silently compared as if they meant the same thing
// (ADR-011). This package only ever produces LatencyWindowedAvg:
// metrics.Sample carries no histogram data, so LatencyHistogram is
// unreachable until a PlatformSource (M7) adds one — the constant exists
// now so Analysis's shape doesn't change when that day comes.
type LatencyStatKind string

const (
	LatencyWindowedAvg LatencyStatKind = "windowed_avg"
	LatencyHistogram   LatencyStatKind = "histogram"
)

// LatencyStat is a percentile plus which method produced it.
type LatencyStat struct {
	P95  time.Duration
	Kind LatencyStatKind
}

// Analysis is the Analyzer's output: the evidence and verdict the Decider
// and the journal need, computed once per disk per cycle. Saturated and
// Underutilized already fold in "sustained over window" (design.md §4.4) —
// callers never re-check a window length or a sample count themselves.
// Everything else is the evidence behind that verdict, for the journal and
// for the Decider's demand estimate (design.md §4.5); Analysis carries no
// demand figure of its own.
type Analysis struct {
	DiskID string

	// HasData is false only when this disk has never had a single sample
	// buffered — the true cold-start case (a fresh disk, a sampler not
	// yet wired, a guest path that never resolved), distinct from Fresh
	// below (pre-merge review: "no_data" and "metrics_stale" are
	// different operational situations and must not share one reason).
	// SampleAge is meaningless when HasData is false — there is no
	// sample to measure the age of — so callers (telemetry.RecordStaleness
	// in particular) must check this before publishing SampleAge, rather
	// than let a zero-value SampleAge be read as "0 seconds old" (which
	// reads as maximally fresh, the opposite of the truth).
	HasData bool

	// Latest-sample snapshot, for the journal — not what Saturated/
	// Underutilized are computed from (each re-evaluates its own signal
	// against every sample in its own window, not just this one).
	// Fresh is false when the newest buffered sample is older than
	// MaxSampleAge — sampling has stopped or fallen badly behind (issue
	// #15). Only meaningful when HasData is true; both are false together
	// for the cold-start case. Saturated and Underutilized are both
	// unconditionally false whenever Fresh is false: stale evidence must
	// never drive a decision, scale-up included. SampleAge is exposed
	// alongside it for the journal and for telemetry.RecordStaleness, not
	// just as an internal gate.
	Fresh     bool
	SampleAge time.Duration

	Source               metrics.SourceKind
	IOPSUtilization      float64
	BandwidthUtilization float64
	ReadOps              float64
	WriteOps             float64
	QueueDepth           metrics.QueueDepth
	ReadLatency          LatencyStat // p95 over ScaleUpWindow
	WriteLatency         LatencyStat
	ThrottleDetected     bool

	Baseline            LatencyStat // low percentile over the full buffered history
	BaselineEstablished bool

	WindowFullUp   bool // span ≥ ScaleUpWindow (ADR-010)
	WindowFullDown bool // span ≥ ScaleDownWindow

	// WindowFillUpRatio/WindowFillDownRatio are the same span-vs-window
	// comparison as WindowFullUp/Down, but as a fraction (capped at 1.0)
	// rather than a threshold boolean — for observability
	// (window_fill_ratio, design.md §7.1), not decision logic. Decide
	// never reads these; it reads the booleans, which are exact ≥
	// comparisons, not a rounded fraction.
	WindowFillUpRatio   float64
	WindowFillDownRatio float64

	Saturated bool // resource_gate AND (queue/latency/throttle) AND sustained (§4.4); false if !WindowFullUp
	// Underutilized requires resource_gate to never trigger (both IOPS and
	// bandwidth utilization below threshold, every sample — the mirror of
	// Saturated's own gate, ADR-008) in addition to every other signal
	// quiet, all sustained over ScaleDownWindow (US-010); false if
	// !WindowFullDown. The resource-gate term exists because queue depth,
	// latency, and throttle detection are all IOPS-denominated and blind
	// to a large-block workload saturating its bandwidth ceiling at a low
	// ops/s count — without it, such a disk could read as quiet on IOPS
	// alone while its bandwidth stayed pinned.
	Underutilized bool

	IOPSP95Down float64 // p95 served ops/s over ScaleDownWindow — observability, and one of EffectiveDemandP95Down's two inputs

	// BandwidthP95DownIOPSEquiv is the p95, over ScaleDownWindow, of served
	// bandwidth converted to IOPS-equivalent units (bytes/s ÷ bytesPerIOPS)
	// — the other input to EffectiveDemandP95Down (issue #17). NBS-PL2
	// ties provisioned IOPS and bandwidth at the same fixed ratio
	// (docs/platform-reference.md, already used for BandwidthUtilization/ADR-008's
	// saturation gate) — a scale-down target sized from ops alone can
	// undersize the bandwidth ceiling a large-block, low-ops/s workload
	// still needs.
	BandwidthP95DownIOPSEquiv float64

	// EffectiveDemandP95Down is the p95, over ScaleDownWindow, of each
	// sample's own max(ops, bandwidth-equivalent) — not
	// max(IOPSP95Down, BandwidthP95DownIOPSEquiv), which would combine two
	// independently-computed percentiles that may come from different
	// samples entirely (issue #17's own warning: "must preserve aligned
	// per-sample totals rather than combining unrelated percentiles").
	// This is what the Decider's scale-down target is actually sized
	// from; IOPSP95Down and BandwidthP95DownIOPSEquiv above stay as
	// separate, independently-meaningful figures for the journal.
	EffectiveDemandP95Down float64

	// LimitingResourceDown names whichever resource IOPSP95Down/
	// BandwidthP95DownIOPSEquiv suggests is currently binding for this
	// disk's down-window — "iops" or "bandwidth" — for the journal
	// (issue #17: "records... the limiting resource"). Informational only,
	// not part of the guardrail math itself.
	LimitingResourceDown string

	// demand()'s (design.md §4.5) two remaining inputs beyond what's above
	// — both measured over ScaleUpWindow, since demand() is only ever
	// called from the Saturated branch. ServedIOPS is exposed directly
	// (not just IOPSUtilization, a ratio) so the Decider never needs
	// specIOPS a second time. QueueGrowthRate is (newest−oldest queue
	// depth) / elapsed seconds within the up-window.
	//
	// QueueGrowthRate is NOT unconditionally safe to sum with ServedIOPS
	// (issue #18): QueueDepth.Unit distinguishes ops (guest) from bytes
	// (platform, docs/platform-reference.md), and the open-workload demand formula is
	// dimensionally valid only for ops/s. QueueGrowthRateValid is true
	// only when every sample in the up-window carried QueueDepthOps; the
	// Decider must check it before adding QueueGrowthRate to ServedIOPS,
	// falling back to the closed-workload (latency-ratio) regime
	// otherwise, same as it already does when no baseline is available.
	// Guest is the only source implemented today and always produces
	// QueueDepthOps, so QueueGrowthRateValid is unconditionally true
	// through the real pipeline right now — the flag exists so a future
	// platform-sourced (bytes) window can never silently reach the
	// ops-denominated formula the moment M7 lands.
	ServedIOPS           float64
	QueueGrowthRate      float64
	QueueGrowthRateValid bool
}

// Analyzer is implemented by WindowAnalyzer (analyzer.go); an interface so
// M4's Decider tests can substitute a fixed Analysis without a real window
// of samples, the same fake-over-mock style internal/compute and
// internal/metrics already use.
type Analyzer interface {
	// diskID is explicit rather than read from samples[0].DiskID — the
	// empty/partial-window case has no samples to read it from, and
	// Analysis.DiskID must still identify which disk a "no data yet"
	// result belongs to.
	// specIOPS is the disk's current provisioned limit (Discover, M1) —
	// needed for utilization and throttling_detected's "served ≈
	// provisioned_limit" comparison. It is live disk state, not a policy
	// value, so it is not part of EvaluationPolicy.
	// now is wall clock, explicit for the same testability reason
	// decider.Decide takes it — see Analyze's own doc comment (issue #15).
	Analyze(diskID string, samples []metrics.Sample, p EvaluationPolicy, specIOPS int64, now time.Time) Analysis
}
