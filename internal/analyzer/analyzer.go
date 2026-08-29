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
// (Sampler's own documented contract) and belong to one disk. now is wall
// clock, explicit for the same reason decider.Decide takes it — freshness
// (out.Fresh, below) is a comparison against real time passing, not
// against anything derivable from the samples themselves (issue #15): a
// disk whose sampling has silently stopped keeps a fixed, internally
// consistent buffer forever, so nothing inside that buffer alone can ever
// reveal that it has gone stale.
func (a *WindowAnalyzer) Analyze(diskID string, samples []metrics.Sample, p EvaluationPolicy, specIOPS int64, now time.Time) Analysis {
	out := Analysis{DiskID: diskID}
	if len(samples) == 0 {
		return out
	}

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
		sustainedQueueBelow(downWindow, p.QueueDepthThreshold) &&
		latencyAtBaseline &&
		!sustainedThrottle(downWindow, specIOPS)

	out.IOPSP95Down, out.BandwidthP95DownIOPSEquiv, out.EffectiveDemandP95Down, out.LimitingResourceDown =
		demandP95Down(downWindow)

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
	stat, ok := computeBaseline(samples)
	if !ok {
		// Full window, but every sample in it was idle in both directions
		// (issue #9) — there is still nothing honest to establish from.
		// Leave established false and retry next cycle, rather than
		// freezing a bogus 0 baseline in place forever (ADR-012's "seeds
		// from the disk's first full window" assumes that window has real
		// latency data to seed from).
		return LatencyStat{}, false
	}
	a.baselines[diskID] = stat
	a.established[diskID] = true
	return stat, true
}

// updateBaseline recomputes and stores diskID's baseline from the current
// samples. Only called when the disk is not currently saturated. Skips the
// write entirely when computeBaseline finds no qualifying sample (issue
// #9) — an idle window must not overwrite a real, previously-established
// baseline with a bogus 0.
func (a *WindowAnalyzer) updateBaseline(diskID string, samples []metrics.Sample) {
	stat, ok := computeBaseline(samples)
	if !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.baselines[diskID] = stat
}

// computeBaseline excludes an idle direction's phantom zero the same way
// readLatencies/writeLatencies do (issue #9). Its second return is false
// when neither direction had any qualifying sample in the window (a
// totally idle window) — establishing or overwriting the baseline with a
// bogus 0 in that case would make every future latency reading look
// "elevated" by comparison, the opposite of what a baseline is for.
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
// minSamplesForWindow floor (ADR-010; design.md §4.3) — and, per issue #15
// / issue #7, also requires that no gap between consecutive samples inside
// the window exceeds maxGap. A window whose overall span covers `window`
// but contains one large silent gap (a missed collection cycle, a source
// outage that later recovered) is not the sustained, gap-free evidence
// design.md §4.4 means by "full".
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

// readLatencies/writeLatencies only include samples where that direction
// actually served ops that interval (issue #9). metrics.computeSample only
// sets ReadLatAvg/WriteLatAvg when the corresponding op-delta was >0 —
// otherwise the field is the Go zero value, not a real "0ns latency"
// observation. Treating an idle direction's zero as real data pollutes
// both the p95 for that direction and, through computeBaseline below, the
// shared latency baseline every direction is compared against.
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

// demandP95Down computes the three down-window demand figures issue #17
// requires: IOPS p95, bandwidth-equivalent p95, and the properly-aligned
// effective-demand p95 those two must not be confused with.
//
// effectiveDemand is deliberately p95(per-sample max(ops, bandwidth-
// equivalent)), not max(p95(ops), p95(bandwidth-equivalent)) — the latter
// would combine two independently-computed percentiles that can come from
// entirely different samples, which is not a value any single moment of
// the workload ever actually produced. Computing the max first, per
// sample, then taking one percentile of that combined series is what
// keeps the demand figure aligned to something the disk was actually
// asked to do at one point in time.
//
// iopsP95/bwP95 are still computed (and returned) independently, purely
// for the journal's own "IOPS demand, bandwidth-equivalent demand"
// figures (issue #17) — decider.Decide's actual scale-down target is
// sized from effectiveDemand alone, never from these two directly.
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
