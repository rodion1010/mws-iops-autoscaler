// Package analyzer turns buffered disk metrics into windowed evidence and verdicts.
// It retains only per-disk latency baselines; target selection belongs to decider.
package analyzer

import (
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

// EvaluationPolicy is the analyzer-specific view of resolved policy.
// Keeping this type local avoids coupling analysis to config loading.
type EvaluationPolicy struct {
	ScaleUpWindow             time.Duration
	ScaleDownWindow           time.Duration
	UtilizationThreshold      float64
	QueueDepthThreshold       float64
	LatencyBaselineMultiplier float64

	// MaxSampleAge marks a non-empty buffer stale when collection stops advancing.
	MaxSampleAge time.Duration

	// MaxSampleGap prevents a window with a long internal outage from counting as full.
	MaxSampleGap time.Duration
}

// LatencyStatKind identifies how a latency percentile was derived.
// Histogram values are reserved for a future source that provides histogram data.
type LatencyStatKind string

const (
	LatencyWindowedAvg LatencyStatKind = "windowed_avg"
	LatencyHistogram   LatencyStatKind = "histogram"
)

// LatencyStat is a percentile together with its derivation method.
type LatencyStat struct {
	P95  time.Duration
	Kind LatencyStatKind
}

// Analysis contains one disk's windowed evidence and verdict for a cycle.
// Saturated and Underutilized already include sustained-window evaluation.
type Analysis struct {
	DiskID string

	// HasData distinguishes cold start from previously available data becoming stale.
	// SampleAge is meaningful only when HasData is true.
	HasData bool

	// Sample is the latest snapshot for diagnostics, not the sole verdict input.
	// Fresh gates all decisions and is meaningful only when HasData is true.
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

	WindowFullUp   bool // span ≥ ScaleUpWindow
	WindowFullDown bool // span ≥ ScaleDownWindow

	// WindowFill ratios expose span progress; decision logic uses the exact WindowFull booleans.
	WindowFillUpRatio   float64
	WindowFillDownRatio float64

	Saturated bool // resource_gate AND (queue/latency/throttle) AND sustained; false if !WindowFullUp
	// Underutilized requires every down-window signal, including both resource utilizations,
	// to remain quiet; bandwidth prevents large-block workloads from appearing idle.
	Underutilized bool

	IOPSP95Down float64 // p95 served ops/s over ScaleDownWindow — observability, and one of EffectiveDemandP95Down's two inputs

	// BandwidthP95DownIOPSEquiv converts down-window bandwidth demand using the platform ratio.
	// It protects target sizing for large-block, low-IOPS workloads.
	BandwidthP95DownIOPSEquiv float64

	// EffectiveDemandP95Down is p95 of each sample's max(IOPS, bandwidth-equivalent demand).
	// Computing the maximum per sample preserves temporal alignment; independent p95s remain diagnostic.
	EffectiveDemandP95Down float64

	// LimitingResourceDown names the larger independent down-window resource estimate.
	// It is informational and does not alter guardrail math.
	LimitingResourceDown string

	// ServedIOPS and QueueGrowthRate feed scale-up demand estimation.
	// QueueGrowthRateValid is true only for ops-denominated queue samples; otherwise the
	// decider must avoid adding it to ServedIOPS and use the closed-workload fallback.
	ServedIOPS           float64
	QueueGrowthRate      float64
	QueueGrowthRateValid bool
}

// Analyzer converts chronological samples into one disk analysis.
// The interface permits deterministic substitutes in decider and agent tests.
type Analyzer interface {
	// diskID remains explicit for empty windows; specIOPS is live provisioned capacity;
	// now is explicit wall clock for freshness evaluation.
	Analyze(diskID string, samples []metrics.Sample, p EvaluationPolicy, specIOPS int64, now time.Time) Analysis
}
