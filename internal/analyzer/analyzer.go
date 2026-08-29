package analyzer

import (
	"sync"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

// bytesPerIOPS is NBS-PL2's fixed IOPS-to-bandwidth ratio, measured in
// CLAUDE.md: "compute_vm_disk_read_latency..." aside, the throughput figure
// itself is `MB/s = IOPS × 32 / 1024`, i.e. exactly 32768 bytes per IOPS
// unit (33554432 Bps at 1000 IOPS). Used for bandwidth_utilization (ADR-008).
const bytesPerIOPS = 32768

// minSamplesForWindow is a floor beneath which a window is never "full"
// even if its timestamp span already covers the configured duration — two
// samples with a large gap between them (a missed tick, a slow scrape) can
// span the right amount of wall-clock time while carrying almost no real
// information. Not a config field: like BillingStepIOPS, this is a fixed
// implementation constant, calibration-pending per design.md §8.5.
const minSamplesForWindow = 3

// baselinePercentile is the "low percentile" design.md §4.4 calls the
// latency baseline, without pinning an exact number. p10 is chosen as low
// enough to represent the disk's near-unloaded service time (most requests
// still complete fast even under moderate load) while not being a single
// unrepresentative low outlier the way p1 or a bare minimum would be.
// Calibration-pending per design.md §8.5, same as the constants above.
const baselinePercentile = 0.10

// throttleTolerance is how close "served" must be to "provisioned" to call
// it throttled (§4.5: "served_iops ≈ provisioned_limit (within tolerance)").
// 2% absorbs ordinary measurement noise around the cap without requiring an
// exact match, which a rate-limited device essentially never produces.
const throttleTolerance = 0.02

// WindowAnalyzer is the concrete Analyzer (design.md §5.2). It is stateful
// across calls — unlike Sample, which is a pure snapshot — because the
// latency baseline must be remembered and selectively frozen (ADR-012), not
// recomputed fresh from whatever happens to be in the current sample
// window. State is keyed by disk ID, mirroring Sampler's own per-disk map.
type WindowAnalyzer struct {
	mu          sync.Mutex
	baselines   map[string]LatencyStat
	established map[string]bool
}

func NewWindowAnalyzer() *WindowAnalyzer {
	return &WindowAnalyzer{
		baselines:   make(map[string]LatencyStat),
		established: make(map[string]bool),
	}
}

// Analyze implements Analyzer. samples must be chronological ascending
// (Sampler's own documented contract) and belong to one disk.
func (a *WindowAnalyzer) Analyze(diskID string, samples []metrics.Sample, p EvaluationPolicy, specIOPS int64) Analysis {
	out := Analysis{DiskID: diskID}
	if len(samples) == 0 {
		return out
	}

	newest := samples[len(samples)-1]
	upWindow := trailingWindow(samples, p.ScaleUpWindow)
	downWindow := trailingWindow(samples, p.ScaleDownWindow)

	out.WindowFullUp = windowFull(samples, p.ScaleUpWindow)
	out.WindowFullDown = windowFull(samples, p.ScaleDownWindow)
	out.WindowFillUpRatio = fillRatio(samples, p.ScaleUpWindow)
	out.WindowFillDownRatio = fillRatio(samples, p.ScaleDownWindow)

	out.Source = newest.Source
	out.IOPSUtilization = iopsUtilization(newest, specIOPS)
	out.BandwidthUtilization = bandwidthUtilization(newest, specIOPS)
	out.ReadOps = newest.ReadOps
	out.WriteOps = newest.WriteOps
	out.QueueDepth = newest.QueueDepth

	out.ReadLatency = LatencyStat{P95: percentile(readLatencies(upWindow), 0.95), Kind: LatencyWindowedAvg}
	out.WriteLatency = LatencyStat{P95: percentile(writeLatencies(upWindow), 0.95), Kind: LatencyWindowedAvg}

	baseline, established := a.currentBaseline(diskID, samples, out.WindowFullUp)
	out.Baseline = baseline
	out.BaselineEstablished = established

	out.ThrottleDetected = sustainedThrottle(upWindow, specIOPS)

	latencyElevated := established && (out.ReadLatency.P95 > scaledDuration(baseline.P95, p.LatencyBaselineMultiplier) ||
		out.WriteLatency.P95 > scaledDuration(baseline.P95, p.LatencyBaselineMultiplier))

	out.Saturated = out.WindowFullUp &&
		sustainedResourceGate(upWindow, specIOPS, p.UtilizationThreshold) &&
		(sustainedQueueAtOrAbove(upWindow, p.QueueDepthThreshold) ||
			latencyElevated ||
			out.ThrottleDetected)

	downReadP95 := percentile(readLatencies(downWindow), 0.95)
	downWriteP95 := percentile(writeLatencies(downWindow), 0.95)
	latencyAtBaseline := !established ||
		(downReadP95 <= scaledDuration(baseline.P95, p.LatencyBaselineMultiplier) &&
			downWriteP95 <= scaledDuration(baseline.P95, p.LatencyBaselineMultiplier))

	out.Underutilized = out.WindowFullDown &&
		sustainedQueueBelow(downWindow, p.QueueDepthThreshold) &&
		latencyAtBaseline &&
		!sustainedThrottle(downWindow, specIOPS)

	out.IOPSP95Down = percentile95TotalOps(downWindow)

	out.ServedIOPS = newest.ReadOps + newest.WriteOps
	out.QueueGrowthRate = queueGrowthRate(upWindow)

	// Freeze against updates while saturated (§4.4) — use this cycle's own
	// verdict, computed above from the baseline as of *before* this
	// update, so an update never retroactively changes the value that
	// decided whether to update.
	if !out.Saturated {
		a.updateBaseline(diskID, samples)
	}

	return out
}

// currentBaseline returns the stored baseline for diskID, establishing it
// now — from samples' own low-percentile latency — the first time
// windowFullUp is true, regardless of whether that window happened to be
// saturated (ADR-012: "seeds from the disk's first full window", never
// gated by saturation). Before the first full window, there is nothing
// honest to establish from yet, so BaselineEstablished stays false.
func (a *WindowAnalyzer) currentBaseline(diskID string, samples []metrics.Sample, windowFullUp bool) (LatencyStat, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.established[diskID] {
		return a.baselines[diskID], true
	}
	if !windowFullUp {
		return LatencyStat{}, false
	}
	stat := computeBaseline(samples)
	a.baselines[diskID] = stat
	a.established[diskID] = true
	return stat, true
}

// updateBaseline recomputes and stores diskID's baseline from the current
// samples. Only called when the disk is not currently saturated.
func (a *WindowAnalyzer) updateBaseline(diskID string, samples []metrics.Sample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.baselines[diskID] = computeBaseline(samples)
}

func computeBaseline(samples []metrics.Sample) LatencyStat {
	all := make([]time.Duration, 0, len(samples)*2)
	for _, s := range samples {
		all = append(all, s.ReadLatAvg, s.WriteLatAvg)
	}
	return LatencyStat{P95: percentile(all, baselinePercentile), Kind: LatencyWindowedAvg}
}

// trailingWindow returns the suffix of samples (chronological ascending)
// whose Timestamp falls within window of the newest sample.
func trailingWindow(samples []metrics.Sample, window time.Duration) []metrics.Sample {
	if len(samples) == 0 {
		return nil
	}
	cutoff := samples[len(samples)-1].Timestamp.Add(-window)
	i := 0
	for ; i < len(samples); i++ {
		if !samples[i].Timestamp.Before(cutoff) {
			break
		}
	}
	return samples[i:]
}

// windowFull is timestamp-span based, not a sample count, beyond the
// minSamplesForWindow floor (ADR-010; design.md §4.3).
func windowFull(samples []metrics.Sample, window time.Duration) bool {
	if len(samples) < minSamplesForWindow {
		return false
	}
	span := samples[len(samples)-1].Timestamp.Sub(samples[0].Timestamp)
	return span >= window
}

// fillRatio is windowFull's fraction form, for observability
// (window_fill_ratio) — capped at 1.0 rather than uncapped, since "more
// than full" isn't a meaningful signal to export. Does not apply
// minSamplesForWindow's floor: that floor exists to keep windowFull from
// declaring readiness on a degenerate two-point span, a decision-logic
// concern; the fill ratio is a plain progress indicator and stays
// proportional even below that floor.
func fillRatio(samples []metrics.Sample, window time.Duration) float64 {
	if len(samples) == 0 || window <= 0 {
		return 0
	}
	span := samples[len(samples)-1].Timestamp.Sub(samples[0].Timestamp)
	ratio := span.Seconds() / window.Seconds()
	if ratio > 1 {
		ratio = 1
	}
	if ratio < 0 {
		ratio = 0
	}
	return ratio
}

func iopsUtilization(s metrics.Sample, specIOPS int64) float64 {
	if specIOPS <= 0 {
		return 0
	}
	return (s.ReadOps + s.WriteOps) / float64(specIOPS)
}

func bandwidthUtilization(s metrics.Sample, specIOPS int64) float64 {
	if specIOPS <= 0 {
		return 0
	}
	return (s.ReadBytes + s.WriteBytes) / (float64(specIOPS) * bytesPerIOPS)
}

func sustainedResourceGate(window []metrics.Sample, specIOPS int64, threshold float64) bool {
	if len(window) == 0 {
		return false
	}
	for _, s := range window {
		gate := iopsUtilization(s, specIOPS) >= threshold || bandwidthUtilization(s, specIOPS) >= threshold
		if !gate {
			return false
		}
	}
	return true
}

func sustainedQueueAtOrAbove(window []metrics.Sample, threshold float64) bool {
	if len(window) == 0 {
		return false
	}
	for _, s := range window {
		if s.QueueDepth.Value < threshold {
			return false
		}
	}
	return true
}

func sustainedQueueBelow(window []metrics.Sample, threshold float64) bool {
	if len(window) == 0 {
		return false
	}
	for _, s := range window {
		if s.QueueDepth.Value >= threshold {
			return false
		}
	}
	return true
}

// sustainedThrottle implements §4.5's throttled := served_iops ≈
// provisioned_limit (within tolerance) AND queue_depth sustained > 0,
// evaluated per-sample across window (its own "sustained" is folded in
// this way, rather than a separately-windowed check — design.md §4.4).
func sustainedThrottle(window []metrics.Sample, specIOPS int64) bool {
	if len(window) == 0 || specIOPS <= 0 {
		return false
	}
	floor := float64(specIOPS) * (1 - throttleTolerance)
	for _, s := range window {
		served := s.ReadOps + s.WriteOps
		if served < floor || s.QueueDepth.Value <= 0 {
			return false
		}
	}
	return true
}

func readLatencies(window []metrics.Sample) []time.Duration {
	out := make([]time.Duration, len(window))
	for i, s := range window {
		out[i] = s.ReadLatAvg
	}
	return out
}

func writeLatencies(window []metrics.Sample) []time.Duration {
	out := make([]time.Duration, len(window))
	for i, s := range window {
		out[i] = s.WriteLatAvg
	}
	return out
}

// queueGrowthRate is (newest−oldest queue depth) / elapsed seconds within
// window — a simple endpoint slope, not a least-squares regression. With
// as few as minSamplesForWindow points, a regression wouldn't be
// meaningfully more accurate than the direct endpoint difference, and the
// endpoint form is far easier to reason about and test. Zero (or negative,
// for two samples with a non-monotonic timestamp — shouldn't happen given
// Sampler's chronological contract, but division still needs a guard) for
// a window that can't produce a rate.
func queueGrowthRate(window []metrics.Sample) float64 {
	if len(window) < 2 {
		return 0
	}
	first, last := window[0], window[len(window)-1]
	elapsed := last.Timestamp.Sub(first.Timestamp).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return (last.QueueDepth.Value - first.QueueDepth.Value) / elapsed
}

func percentile95TotalOps(window []metrics.Sample) float64 {
	if len(window) == 0 {
		return 0
	}
	ops := make([]float64, len(window))
	for i, s := range window {
		ops[i] = s.ReadOps + s.WriteOps
	}
	return percentileFloat(ops, 0.95)
}

func scaledDuration(d time.Duration, mult float64) time.Duration {
	return time.Duration(float64(d) * mult)
}
