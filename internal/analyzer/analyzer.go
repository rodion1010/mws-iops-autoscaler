package analyzer

import (
	"sync"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

// bytesPerIOPS is the platform's fixed 32 KiB-per-IOPS conversion.
const bytesPerIOPS = 32768

// minSamplesForWindow prevents sparse two-point spans from satisfying a window.
const minSamplesForWindow = 3

// baselinePercentile uses p10 as a stable near-unloaded latency estimate.
const baselinePercentile = 0.10

// throttleTolerance absorbs measurement noise near the provisioned limit.
const throttleTolerance = 0.02

// WindowAnalyzer evaluates rolling windows and retains per-disk latency baselines.
// Baselines are stateful because saturation freezes their updates.
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

// Analyze evaluates chronological samples for one disk.
// Explicit now makes source staleness observable even when the buffer stops changing.
func (a *WindowAnalyzer) Analyze(diskID string, samples []metrics.Sample, p EvaluationPolicy, specIOPS int64, now time.Time) Analysis {
	out := Analysis{DiskID: diskID}
	if len(samples) == 0 {
		// Cold start is distinct from previously available data becoming stale.
		return out
	}
	out.HasData = true

	newest := samples[len(samples)-1]
	upWindow := trailingWindow(samples, p.ScaleUpWindow)
	downWindow := trailingWindow(samples, p.ScaleDownWindow)

	out.SampleAge = now.Sub(newest.Timestamp)
	out.Fresh = out.SampleAge <= p.MaxSampleAge

	out.WindowFullUp = windowFull(upWindow, p.ScaleUpWindow, p.MaxSampleGap)
	out.WindowFullDown = windowFull(downWindow, p.ScaleDownWindow, p.MaxSampleGap)
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

	out.Saturated = out.Fresh && out.WindowFullUp &&
		sustainedResourceGate(upWindow, specIOPS, p.UtilizationThreshold) &&
		(sustainedQueueAtOrAbove(upWindow, p.QueueDepthThreshold) ||
			latencyElevated ||
			out.ThrottleDetected)

	downReadP95 := percentile(readLatencies(downWindow), 0.95)
	downWriteP95 := percentile(writeLatencies(downWindow), 0.95)
	latencyAtBaseline := !established ||
		(downReadP95 <= scaledDuration(baseline.P95, p.LatencyBaselineMultiplier) &&
			downWriteP95 <= scaledDuration(baseline.P95, p.LatencyBaselineMultiplier))

	out.Underutilized = out.Fresh && out.WindowFullDown &&
		sustainedResourceBelow(downWindow, specIOPS, p.UtilizationThreshold) &&
		sustainedQueueBelow(downWindow, p.QueueDepthThreshold) &&
		latencyAtBaseline &&
		!sustainedThrottle(downWindow, specIOPS)

	out.IOPSP95Down, out.BandwidthP95DownIOPSEquiv, out.EffectiveDemandP95Down, out.LimitingResourceDown =
		demandP95Down(downWindow)

	out.ServedIOPS = newest.ReadOps + newest.WriteOps
	out.QueueGrowthRate, out.QueueGrowthRateValid = queueGrowthRate(upWindow)

	// Use the pre-update baseline to decide whether this cycle may update it.
	if !out.Saturated {
		a.updateBaseline(diskID, samples)
	}

	return out
}

// currentBaseline seeds a disk's baseline from its first full window with latency data.
// Until then BaselineEstablished remains false.
func (a *WindowAnalyzer) currentBaseline(diskID string, samples []metrics.Sample, windowFullUp bool) (LatencyStat, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.established[diskID] {
		return a.baselines[diskID], true
	}
	if !windowFullUp {
		return LatencyStat{}, false
	}
	stat, ok := computeBaseline(samples)
	if !ok {
		// An entirely idle window has no latency data from which to seed a baseline.
		return LatencyStat{}, false
	}
	a.baselines[diskID] = stat
	a.established[diskID] = true
	return stat, true
}

// updateBaseline stores a new baseline only when qualifying latency data exists.
// Idle windows therefore cannot overwrite an established value with zero.
func (a *WindowAnalyzer) updateBaseline(diskID string, samples []metrics.Sample) {
	stat, ok := computeBaseline(samples)
	if !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.baselines[diskID] = stat
}

// computeBaseline ignores zero-value latency from idle directions.
// Its boolean result distinguishes an idle window from a real zero estimate.
func computeBaseline(samples []metrics.Sample) (LatencyStat, bool) {
	all := make([]time.Duration, 0, len(samples)*2)
	for _, s := range samples {
		if s.ReadOps > 0 {
			all = append(all, s.ReadLatAvg)
		}
		if s.WriteOps > 0 {
			all = append(all, s.WriteLatAvg)
		}
	}
	if len(all) == 0 {
		return LatencyStat{}, false
	}
	return LatencyStat{P95: percentile(all, baselinePercentile), Kind: LatencyWindowedAvg}, true
}

// trailingWindow returns the natural time suffix plus one predecessor only when
// scheduler jitter leaves that suffix just short of a full span.
// windowFull still rejects a genuine discontinuity through maxGap.
func trailingWindow(samples []metrics.Sample, window time.Duration) []metrics.Sample {
	if len(samples) == 0 {
		return nil
	}
	newest := samples[len(samples)-1].Timestamp
	cutoff := newest.Add(-window)
	i := 0
	for ; i < len(samples); i++ {
		if !samples[i].Timestamp.Before(cutoff) {
			break
		}
	}
	if i > 0 && newest.Sub(samples[i].Timestamp) < window {
		i--
	}
	return samples[i:]
}

// windowFull requires enough samples, a complete timestamp span, and no gap above maxGap.
// This rejects sparse data that merely covers the required wall-clock duration.
func windowFull(samples []metrics.Sample, window time.Duration, maxGap time.Duration) bool {
	if len(samples) < minSamplesForWindow {
		return false
	}
	span := samples[len(samples)-1].Timestamp.Sub(samples[0].Timestamp)
	if span < window {
		return false
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].Timestamp.Sub(samples[i-1].Timestamp) > maxGap {
			return false
		}
	}
	return true
}

// fillRatio reports time-span progress capped at 1.
// Sample-count readiness is a decision concern and is intentionally not folded into this gauge.
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

// sustainedResourceBelow requires every sample to be below both IOPS and bandwidth thresholds.
// Checking bandwidth prevents large-block workloads from appearing quiet at low operations per second.
func sustainedResourceBelow(window []metrics.Sample, specIOPS int64, threshold float64) bool {
	if len(window) == 0 {
		return false
	}
	for _, s := range window {
		if iopsUtilization(s, specIOPS) >= threshold || bandwidthUtilization(s, specIOPS) >= threshold {
			return false
		}
	}
	return true
}

// queueUnitConsistent protects ops-denominated formulas from byte-denominated queue data.
// Unknown or mixed units fail closed.
func queueUnitConsistent(window []metrics.Sample, want metrics.QueueDepthUnit) bool {
	for _, s := range window {
		if s.QueueDepth.Unit != want {
			return false
		}
	}
	return true
}

func sustainedQueueAtOrAbove(window []metrics.Sample, threshold float64) bool {
	if len(window) == 0 || !queueUnitConsistent(window, metrics.QueueDepthOps) {
		return false
	}
	for _, s := range window {
		if s.QueueDepth.Value < threshold {
			return false
		}
	}
	return true
}

// sustainedQueueBelow also requires ops-denominated queue data, so unknown units
// cannot qualify a disk as underutilized.
func sustainedQueueBelow(window []metrics.Sample, threshold float64) bool {
	if len(window) == 0 || !queueUnitConsistent(window, metrics.QueueDepthOps) {
		return false
	}
	for _, s := range window {
		if s.QueueDepth.Value >= threshold {
			return false
		}
	}
	return true
}

// sustainedThrottle requires every sample to be near the IOPS limit with a non-empty queue.
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

// readLatencies and writeLatencies ignore idle directions whose zero value is not an observation.
// This keeps direction p95s and the shared baseline free of phantom zero latency.
func readLatencies(window []metrics.Sample) []time.Duration {
	out := make([]time.Duration, 0, len(window))
	for _, s := range window {
		if s.ReadOps > 0 {
			out = append(out, s.ReadLatAvg)
		}
	}
	return out
}

func writeLatencies(window []metrics.Sample) []time.Duration {
	out := make([]time.Duration, 0, len(window))
	for _, s := range window {
		if s.WriteOps > 0 {
			out = append(out, s.WriteLatAvg)
		}
	}
	return out
}

// queueGrowthRate is the endpoint queue-depth slope in ops per second.
// It is invalid for non-ops units or non-positive elapsed time, which callers must reject.
func queueGrowthRate(window []metrics.Sample) (rate float64, valid bool) {
	if len(window) < 2 || !queueUnitConsistent(window, metrics.QueueDepthOps) {
		return 0, false
	}
	first, last := window[0], window[len(window)-1]
	elapsed := last.Timestamp.Sub(first.Timestamp).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	return (last.QueueDepth.Value - first.QueueDepth.Value) / elapsed, true
}

// demandP95Down returns IOPS, bandwidth-equivalent, and effective demand p95s.
// Effective demand is p95 of each sample's resource maximum, preserving temporal alignment;
// the independent p95s are retained only for observability.
func demandP95Down(window []metrics.Sample) (iopsP95, bwP95, effectiveDemand float64, limitingResource string) {
	if len(window) == 0 {
		return 0, 0, 0, ""
	}
	ops := make([]float64, len(window))
	bw := make([]float64, len(window))
	eff := make([]float64, len(window))
	for i, s := range window {
		ops[i] = s.ReadOps + s.WriteOps
		bw[i] = (s.ReadBytes + s.WriteBytes) / bytesPerIOPS
		if bw[i] > ops[i] {
			eff[i] = bw[i]
		} else {
			eff[i] = ops[i]
		}
	}

	iopsP95 = percentileFloat(ops, 0.95)
	bwP95 = percentileFloat(bw, 0.95)
	effectiveDemand = percentileFloat(eff, 0.95)
	limitingResource = "iops"
	if bwP95 > iopsP95 {
		limitingResource = "bandwidth"
	}
	return iopsP95, bwP95, effectiveDemand, limitingResource
}

func scaledDuration(d time.Duration, mult float64) time.Duration {
	return time.Duration(float64(d) * mult)
}
