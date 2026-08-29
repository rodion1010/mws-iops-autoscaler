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

	// Latest-sample snapshot, for the journal — not what Saturated/
	// Underutilized are computed from (each re-evaluates its own signal
	// against every sample in its own window, not just this one).
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

	Saturated     bool // resource_gate AND (queue/latency/throttle) AND sustained (§4.4); false if !WindowFullUp
	Underutilized bool // every signal quiet AND sustained over ScaleDownWindow (US-010); false if !WindowFullDown

	IOPSP95Down float64 // p95 served ops/s over ScaleDownWindow — the Decider's p95_iops(m) input

	// demand()'s (design.md §4.5) two remaining inputs beyond what's above
	// — both measured over ScaleUpWindow, since demand() is only ever
	// called from the Saturated branch. ServedIOPS is exposed directly
	// (not just IOPSUtilization, a ratio) so the Decider never needs
	// specIOPS a second time. QueueGrowthRate is (newest−oldest queue
	// depth) / elapsed seconds within the up-window — units match
	// QueueDepth.Unit per second, so it sums directly with ServedIOPS in
	// the open-workload form.
	ServedIOPS      float64
	QueueGrowthRate float64
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
	Analyze(diskID string, samples []metrics.Sample, p EvaluationPolicy, specIOPS int64) Analysis
}
