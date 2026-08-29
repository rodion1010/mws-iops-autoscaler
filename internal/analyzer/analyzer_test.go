package analyzer

import (
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

const specIOPS = int64(1000)

var testPolicy = EvaluationPolicy{
	ScaleUpWindow:             5 * time.Minute,
	ScaleDownWindow:           30 * time.Minute,
	UtilizationThreshold:      0.98,
	QueueDepthThreshold:       1,
	LatencyBaselineMultiplier: 1.5,
	// Generous enough that ordinary tests (samples one minute apart, `now`
	// taken as the newest sample's own timestamp) never trip freshness or
	// gap checks by accident — issues #15/#7 get their own dedicated tests
	// below with tight values instead of tightening this shared policy.
	MaxSampleAge: time.Hour,
	MaxSampleGap: time.Hour,
}

var epoch = time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)

// asOf returns the newest sample's own timestamp — the natural `now` for a
// test that only cares about ordinary (non-staleness) behavior, since it
// makes SampleAge exactly 0 regardless of which epoch offset the samples
// use.
func asOf(samples []metrics.Sample) time.Time {
	return samples[len(samples)-1].Timestamp
}

// sampleAt builds one synthetic Sample minute*minutes after epoch.
func sampleAt(minute int, readOps, writeOps, readBytes, writeBytes, queue float64, readLat, writeLat time.Duration) metrics.Sample {
	return metrics.Sample{
		DiskID:      "disk1",
		Timestamp:   epoch.Add(time.Duration(minute) * time.Minute),
		ReadOps:     readOps,
		WriteOps:    writeOps,
		ReadBytes:   readBytes,
		WriteBytes:  writeBytes,
		QueueDepth:  metrics.QueueDepth{Value: queue, Unit: metrics.QueueDepthOps},
		ReadLatAvg:  readLat,
		WriteLatAvg: writeLat,
		Source:      metrics.SourceGuest,
	}
}

// quietUpWindow is 6 samples, one per minute (0..5, span exactly
// ScaleUpWindow), all low utilization, low queue, low flat latency, no
// throttle — the baseline this establishes is ~1ms.
func quietUpWindow() []metrics.Sample {
	out := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		out = append(out, sampleAt(m, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond))
	}
	return out
}

// quietDownWindow is 31 samples, one per minute (0..30, span exactly
// ScaleDownWindow), all quiet — its trailing 6 samples are also a full,
// quiet up-window (up-window ⊆ down-window, both trailing from the newest
// sample), so a single Analyze call over this slice establishes the
// baseline and can be evaluated for both directions at once.
func quietDownWindow() []metrics.Sample {
	out := make([]metrics.Sample, 0, 31)
	for m := 0; m <= 30; m++ {
		out = append(out, sampleAt(m, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond))
	}
	return out
}

func TestAnalyze_emptySamples(t *testing.T) {
	a := NewWindowAnalyzer()
	got := a.Analyze("disk1", nil, testPolicy, specIOPS, epoch)
	if got != (Analysis{DiskID: "disk1"}) {
		t.Errorf("Analyze(empty) = %+v, want zero Analysis with DiskID set", got)
	}
}

func TestAnalyze_partialWindowNeverSaturates(t *testing.T) {
	a := NewWindowAnalyzer()
	// Only 4 samples spanning 3 minutes — short of the 5-minute ScaleUpWindow
	// — even though every sample looks fully saturated.
	samples := []metrics.Sample{
		sampleAt(0, 1000, 0, 0, 0, 5, time.Millisecond, time.Millisecond),
		sampleAt(1, 1000, 0, 0, 0, 5, time.Millisecond, time.Millisecond),
		sampleAt(2, 1000, 0, 0, 0, 5, time.Millisecond, time.Millisecond),
		sampleAt(3, 1000, 0, 0, 0, 5, time.Millisecond, time.Millisecond),
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.WindowFullUp {
		t.Error("WindowFullUp = true, want false (span is 3m, ScaleUpWindow is 5m)")
	}
	if got.Saturated {
		t.Error("Saturated = true, want false — a partial window must never act (NFR-9)")
	}
}

func TestAnalyze_exactlyFullWindowIsFull(t *testing.T) {
	a := NewWindowAnalyzer()
	up := quietUpWindow()
	got := a.Analyze("disk1", up, testPolicy, specIOPS, asOf(up))
	if !got.WindowFullUp {
		t.Error("WindowFullUp = false, want true — span exactly equals ScaleUpWindow (boundary is inclusive)")
	}
}

func TestAnalyze_gatePassesButNoSignal_notSaturated(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		// 1000 ops/s = 100% IOPS utilization (gate met), but queue is
		// empty and latency is flat — efficient, not saturated (§4.4).
		samples = append(samples, sampleAt(m, 1000, 0, 0, 0, 0, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.IOPSUtilization < testPolicy.UtilizationThreshold {
		t.Fatalf("test setup: IOPSUtilization = %v, want >= threshold", got.IOPSUtilization)
	}
	if got.Saturated {
		t.Error("Saturated = true, want false — a disk at 100% utilization with an empty queue and flat latency is efficient (ADR-002)")
	}
}

func TestAnalyze_queueDepthAloneTriggers(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		// Gate satisfied via bandwidth (large-block, low ops/s) so it
		// never crosses the throttle floor on served IOPS — isolates the
		// queue-depth trigger from throttling_detected.
		samples = append(samples, sampleAt(m, 100, 0, 40*1024*1024, 0, 5, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.BandwidthUtilization < testPolicy.UtilizationThreshold {
		t.Fatalf("test setup: BandwidthUtilization = %v, want >= threshold", got.BandwidthUtilization)
	}
	if got.ThrottleDetected {
		t.Fatal("test setup: ThrottleDetected = true, want isolated queue-depth trigger only")
	}
	if !got.Saturated {
		t.Error("Saturated = false, want true — sustained queue depth alone must trigger (§4.4)")
	}
}

func TestAnalyze_latencyAloneTriggers(t *testing.T) {
	a := NewWindowAnalyzer()

	// Cycle 1: quiet, establishes a ~1ms baseline.
	up := quietUpWindow()
	if got := a.Analyze("disk1", up, testPolicy, specIOPS, asOf(up)); got.Saturated {
		t.Fatal("test setup: cycle 1 must not be saturated")
	} else if !got.BaselineEstablished {
		t.Fatal("test setup: baseline must establish from the first full window")
	}

	// Cycle 2: gate met via bandwidth, queue empty, no throttle, but
	// latency well above baseline × multiplier (1ms × 1.5 = 1.5ms).
	samples := make([]metrics.Sample, 0, 6)
	for m := 100; m <= 105; m++ {
		samples = append(samples, sampleAt(m, 100, 0, 40*1024*1024, 0, 0, 5*time.Millisecond, 5*time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.ThrottleDetected {
		t.Fatal("test setup: ThrottleDetected = true, want isolated latency trigger only")
	}
	if !got.BaselineEstablished {
		t.Fatal("test setup: baseline should still be established from cycle 1")
	}
	if !got.Saturated {
		t.Error("Saturated = false, want true — latency sustained above baseline × multiplier must trigger (§4.4)")
	}
}

func TestAnalyze_throttleAloneTriggers(t *testing.T) {
	a := NewWindowAnalyzer()

	// Cycle 1: quiet, establishes baseline.
	up := quietUpWindow()
	a.Analyze("disk1", up, testPolicy, specIOPS, asOf(up))

	// Cycle 2: served pinned at the provisioned limit (990/1000, within
	// throttleTolerance), queue > 0 but below QueueDepthThreshold (1) so
	// the queue-depth trigger itself doesn't independently fire, latency
	// flat at baseline.
	samples := make([]metrics.Sample, 0, 6)
	for m := 100; m <= 105; m++ {
		samples = append(samples, sampleAt(m, 990, 0, 0, 0, 0.5, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.QueueDepth.Value >= testPolicy.QueueDepthThreshold {
		t.Fatalf("test setup: queue = %v, want below threshold to isolate throttling", got.QueueDepth.Value)
	}
	if !got.ThrottleDetected {
		t.Fatal("test setup: ThrottleDetected = false, want true")
	}
	if !got.Saturated {
		t.Error("Saturated = false, want true — throttling_detected alone must trigger (§4.4)")
	}
}

func TestAnalyze_momentarySpikeNotSustained(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		queue := 0.0
		if m == 3 {
			queue = 5 // one spike, mid-window
		}
		samples = append(samples, sampleAt(m, 1000, 0, 0, 0, queue, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.Saturated {
		t.Error("Saturated = true, want false — a single-sample queue spike must not read as sustained saturation")
	}
}

func TestAnalyze_baselineEstablishesEvenWhenSaturatedFromStart(t *testing.T) {
	// ADR-012 regression: a disk saturated on every sample from the first
	// window onward must still establish a baseline, not deadlock forever.
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		samples = append(samples, sampleAt(m, 1000, 0, 0, 0, 5, 10*time.Millisecond, 10*time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if !got.Saturated {
		t.Fatal("test setup: expected this window to be saturated (queue-driven)")
	}
	if !got.BaselineEstablished {
		t.Error("BaselineEstablished = false, want true — establishment must not be blocked by saturation (ADR-012)")
	}
}

func TestAnalyze_baselineFrozenWhileSaturatedThenResumes(t *testing.T) {
	a := NewWindowAnalyzer()

	saturated := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		saturated = append(saturated, sampleAt(m, 1000, 0, 0, 0, 5, 10*time.Millisecond, 10*time.Millisecond))
	}
	first := a.Analyze("disk1", saturated, testPolicy, specIOPS, asOf(saturated))
	if !first.Saturated || !first.BaselineEstablished {
		t.Fatal("test setup: cycle 1 must be saturated with a baseline established")
	}
	baselineAfterCycle1 := first.Baseline.P95

	// Cycle 2: still saturated, but with a much lower latency distribution
	// — if the baseline recomputed from this data, it would change.
	stillSaturated := make([]metrics.Sample, 0, 6)
	for m := 100; m <= 105; m++ {
		stillSaturated = append(stillSaturated, sampleAt(m, 1000, 0, 0, 0, 5, time.Microsecond, time.Microsecond))
	}
	second := a.Analyze("disk1", stillSaturated, testPolicy, specIOPS, asOf(stillSaturated))
	if !second.Saturated {
		t.Fatal("test setup: cycle 2 must still be saturated")
	}
	if second.Baseline.P95 != baselineAfterCycle1 {
		t.Errorf("Baseline.P95 changed from %v to %v while saturated — must be frozen against updates (§4.4)",
			baselineAfterCycle1, second.Baseline.P95)
	}

	// Cycle 3: quiet — updateBaseline fires this cycle (stored for the
	// *next* lookup), but this cycle's own reported Baseline still
	// reflects the value as of before that update — a value must not
	// retroactively describe itself. Cycle 3's own reported Baseline
	// therefore still equals cycle 1's.
	quiet := make([]metrics.Sample, 0, 6)
	for m := 200; m <= 205; m++ {
		quiet = append(quiet, sampleAt(m, 100, 0, 0, 0, 0, time.Microsecond, time.Microsecond))
	}
	third := a.Analyze("disk1", quiet, testPolicy, specIOPS, asOf(quiet))
	if third.Saturated {
		t.Fatal("test setup: cycle 3 must not be saturated")
	}
	if third.Baseline.P95 != baselineAfterCycle1 {
		t.Errorf("cycle 3's own reported Baseline.P95 = %v, want %v (the update it triggers only affects the next lookup)",
			third.Baseline.P95, baselineAfterCycle1)
	}

	// Cycle 4: the freeze must have lifted after cycle 3's quiet update.
	fourth := a.Analyze("disk1", quiet, testPolicy, specIOPS, asOf(quiet))
	if fourth.Baseline.P95 == baselineAfterCycle1 {
		t.Error("Baseline.P95 never updated after saturation cleared — the freeze must lift, not persist forever")
	}
}

func TestAnalyze_underutilizedOnSustainedQuiet(t *testing.T) {
	a := NewWindowAnalyzer()
	down := quietDownWindow()
	got := a.Analyze("disk1", down, testPolicy, specIOPS, asOf(down))
	if !got.WindowFullDown {
		t.Fatal("test setup: expected WindowFullDown = true (span is exactly 30m)")
	}
	if got.Saturated {
		t.Error("Saturated = true, want false for a fully quiet window")
	}
	if !got.Underutilized {
		t.Error("Underutilized = false, want true — every signal quiet, sustained over ScaleDownWindow (US-010)")
	}
}

func TestAnalyze_notUnderutilizedWhenQueueBuiltUpMidWindow(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 31)
	for m := 0; m <= 30; m++ {
		queue := 0.0
		if m == 15 {
			queue = 5 // one non-quiet sample well within the down-window
		}
		samples = append(samples, sampleAt(m, 100, 0, 0, 0, queue, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.Underutilized {
		t.Error("Underutilized = true, want false — one non-quiet sample in the down-window must break sustain")
	}
}

func TestAnalyze_servedIOPSAndQueueGrowthRate(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		// Queue climbs by 1 per minute: 0,1,2,3,4,5 -> growth rate = 5 / 300s.
		samples = append(samples, sampleAt(m, 300, 200, 0, 0, float64(m), time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.ServedIOPS != 500 {
		t.Errorf("ServedIOPS = %v, want 500 (latest sample's ReadOps+WriteOps)", got.ServedIOPS)
	}
	wantRate := 5.0 / 300.0
	if diff := got.QueueGrowthRate - wantRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("QueueGrowthRate = %v, want %v", got.QueueGrowthRate, wantRate)
	}
	if !got.QueueGrowthRateValid {
		t.Error("QueueGrowthRateValid = false, want true — guest samples are always ops-denominated (ADR-001)")
	}
}

// --- issue #18: queue units must never mix into IOPS demand math ---

// sampleWithQueueUnit builds one sample identical in shape to sampleAt but
// with an explicit QueueDepth unit, so these tests can construct a
// bytes-denominated window with the exact same numeric progression as an
// ops-denominated one — proving the two cannot produce the same math
// despite identical raw values.
func sampleWithQueueUnit(minute int, queue float64, unit metrics.QueueDepthUnit) metrics.Sample {
	s := sampleAt(minute, 100, 0, 0, 0, queue, time.Millisecond, time.Millisecond)
	s.QueueDepth.Unit = unit
	return s
}

func TestQueueUnitConsistent_mixedOrBytesWindowRejected(t *testing.T) {
	opsWindow := []metrics.Sample{
		sampleWithQueueUnit(0, 5, metrics.QueueDepthOps),
		sampleWithQueueUnit(1, 5, metrics.QueueDepthOps),
	}
	if !queueUnitConsistent(opsWindow, metrics.QueueDepthOps) {
		t.Error("queueUnitConsistent(all-ops, ops) = false, want true")
	}

	bytesWindow := []metrics.Sample{
		sampleWithQueueUnit(0, 5, metrics.QueueDepthBytes),
		sampleWithQueueUnit(1, 5, metrics.QueueDepthBytes),
	}
	if queueUnitConsistent(bytesWindow, metrics.QueueDepthOps) {
		t.Error("queueUnitConsistent(all-bytes, ops) = true, want false")
	}

	mixedWindow := []metrics.Sample{
		sampleWithQueueUnit(0, 5, metrics.QueueDepthOps),
		sampleWithQueueUnit(1, 5, metrics.QueueDepthBytes),
	}
	if queueUnitConsistent(mixedWindow, metrics.QueueDepthOps) {
		t.Error("queueUnitConsistent(mixed, ops) = true, want false — one wrong-unit sample must invalidate the whole window")
	}
}

// TestSustainedQueue_identicalValuesDifferentUnitsDiverge is the issue's
// own acceptance criterion: "tests cover identical numeric values with ops
// and bytes and prove they cannot produce silently identical math." Both
// windows carry the exact same QueueDepth.Value progression; only the unit
// differs.
func TestSustainedQueue_identicalValuesDifferentUnitsDiverge(t *testing.T) {
	const threshold = 1.0
	opsWindow := []metrics.Sample{
		sampleWithQueueUnit(0, 5, metrics.QueueDepthOps),
		sampleWithQueueUnit(1, 5, metrics.QueueDepthOps),
	}
	bytesWindow := []metrics.Sample{
		sampleWithQueueUnit(0, 5, metrics.QueueDepthBytes),
		sampleWithQueueUnit(1, 5, metrics.QueueDepthBytes),
	}

	if !sustainedQueueAtOrAbove(opsWindow, threshold) {
		t.Error("sustainedQueueAtOrAbove(ops, value=5, threshold=1) = false, want true")
	}
	if sustainedQueueAtOrAbove(bytesWindow, threshold) {
		t.Error("sustainedQueueAtOrAbove(bytes, value=5, threshold=1) = true, want false — an ops-calibrated threshold must never confirm against bytes, even with an identical raw value")
	}

	quietOps := []metrics.Sample{
		sampleWithQueueUnit(0, 0, metrics.QueueDepthOps),
		sampleWithQueueUnit(1, 0, metrics.QueueDepthOps),
	}
	quietBytes := []metrics.Sample{
		sampleWithQueueUnit(0, 0, metrics.QueueDepthBytes),
		sampleWithQueueUnit(1, 0, metrics.QueueDepthBytes),
	}
	if !sustainedQueueBelow(quietOps, threshold) {
		t.Error("sustainedQueueBelow(ops, value=0, threshold=1) = false, want true")
	}
	if sustainedQueueBelow(quietBytes, threshold) {
		t.Error("sustainedQueueBelow(bytes, value=0, threshold=1) = true, want false — a byte-denominated window must fail closed, not be read as confirming quiet")
	}
}

// TestQueueGrowthRate_identicalProgressionDifferentUnitsDiverge is the
// growth-rate half of the same acceptance criterion, at the exact call
// site issue #18 flags: decider.demand()'s open-workload sum.
func TestQueueGrowthRate_identicalProgressionDifferentUnitsDiverge(t *testing.T) {
	opsWindow := []metrics.Sample{
		sampleWithQueueUnit(0, 0, metrics.QueueDepthOps),
		sampleWithQueueUnit(1, 60, metrics.QueueDepthOps),
	}
	rate, valid := queueGrowthRate(opsWindow)
	if !valid {
		t.Fatal("valid = false for an all-ops window, want true")
	}
	if rate != 1.0 {
		t.Errorf("rate = %v, want 1.0 (60 units over 60s)", rate)
	}

	bytesWindow := []metrics.Sample{
		sampleWithQueueUnit(0, 0, metrics.QueueDepthBytes),
		sampleWithQueueUnit(1, 60, metrics.QueueDepthBytes),
	}
	_, valid = queueGrowthRate(bytesWindow)
	if valid {
		t.Error("valid = true for an all-bytes window, want false — a bytes/s rate must never be handed to the Decider labeled as if it were ops/s")
	}
}

func TestAnalyze_sourceAndPerOpOpsAndFillRatio(t *testing.T) {
	a := NewWindowAnalyzer()
	// 4 samples spanning 3 minutes -> 3/5 = 0.6 of ScaleUpWindow(5m),
	// 3/30 = 0.1 of ScaleDownWindow(30m).
	samples := []metrics.Sample{
		sampleAt(0, 300, 200, 0, 0, 0, time.Millisecond, time.Millisecond),
		sampleAt(1, 300, 200, 0, 0, 0, time.Millisecond, time.Millisecond),
		sampleAt(2, 300, 200, 0, 0, 0, time.Millisecond, time.Millisecond),
		sampleAt(3, 300, 200, 0, 0, 0, time.Millisecond, time.Millisecond),
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.Source != metrics.SourceGuest {
		t.Errorf("Source = %v, want %v", got.Source, metrics.SourceGuest)
	}
	if got.ReadOps != 300 || got.WriteOps != 200 {
		t.Errorf("ReadOps/WriteOps = %v/%v, want 300/200 (latest sample)", got.ReadOps, got.WriteOps)
	}
	if diff := got.WindowFillUpRatio - 0.6; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("WindowFillUpRatio = %v, want 0.6", got.WindowFillUpRatio)
	}
	if diff := got.WindowFillDownRatio - 0.1; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("WindowFillDownRatio = %v, want 0.1", got.WindowFillDownRatio)
	}
}

func TestAnalyze_fillRatioCappedAtOne(t *testing.T) {
	a := NewWindowAnalyzer()
	down := quietDownWindow()
	got := a.Analyze("disk1", down, testPolicy, specIOPS, asOf(down)) // span == ScaleDownWindow exactly
	if got.WindowFillUpRatio != 1 {
		t.Errorf("WindowFillUpRatio = %v, want 1 (span exceeds ScaleUpWindow)", got.WindowFillUpRatio)
	}
	if got.WindowFillDownRatio != 1 {
		t.Errorf("WindowFillDownRatio = %v, want 1 (span exactly equals ScaleDownWindow)", got.WindowFillDownRatio)
	}
}

func TestAnalyze_nonPositiveSpecIOPSIsDefensive(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		samples = append(samples, sampleAt(m, 1000, 0, 0, 0, 5, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, 0, asOf(samples))
	if got.IOPSUtilization != 0 || got.BandwidthUtilization != 0 {
		t.Errorf("utilization with specIOPS<=0 = (%v, %v), want (0, 0)", got.IOPSUtilization, got.BandwidthUtilization)
	}
	if got.ThrottleDetected {
		t.Error("ThrottleDetected = true with specIOPS<=0, want false — no provisioned limit to compare against")
	}
}

// --- issue #15: fail closed on stale metrics ---

func TestAnalyze_staleSamplesNeverSaturate(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		// Queue-driven saturation shape — would trigger Saturated if fresh.
		samples = append(samples, sampleAt(m, 1000, 0, 0, 0, 5, time.Millisecond, time.Millisecond))
	}
	policy := testPolicy
	policy.MaxSampleAge = time.Minute
	now := asOf(samples).Add(10 * time.Minute) // sampling has silently stopped
	got := a.Analyze("disk1", samples, policy, specIOPS, now)
	if got.Fresh {
		t.Fatal("test setup: Fresh = true, want false (10m age against a 1m MaxSampleAge)")
	}
	if got.Saturated {
		t.Error("Saturated = true, want false — stale evidence must never drive a decision (issue #15)")
	}
}

func TestAnalyze_staleSamplesNeverUnderutilize(t *testing.T) {
	a := NewWindowAnalyzer()
	down := quietDownWindow()
	policy := testPolicy
	policy.MaxSampleAge = time.Minute
	now := asOf(down).Add(10 * time.Minute)
	got := a.Analyze("disk1", down, policy, specIOPS, now)
	if got.Fresh {
		t.Fatal("test setup: Fresh = true, want false")
	}
	if got.Underutilized {
		t.Error("Underutilized = true, want false — stale evidence must never drive a decision (issue #15)")
	}
}

func TestAnalyze_sampleAgeReportedEvenWhenFresh(t *testing.T) {
	a := NewWindowAnalyzer()
	up := quietUpWindow()
	now := asOf(up).Add(30 * time.Second)
	got := a.Analyze("disk1", up, testPolicy, specIOPS, now)
	if !got.Fresh {
		t.Fatal("test setup: Fresh = false, want true (30s age, 1h MaxSampleAge)")
	}
	if got.SampleAge != 30*time.Second {
		t.Errorf("SampleAge = %v, want 30s", got.SampleAge)
	}
}

// --- issue #7 (reopened): gap-aware window coverage ---

func TestWindowFull_gapBreaksFullness(t *testing.T) {
	// Overall span covers the window and the sample count clears
	// minSamplesForWindow, but one gap between samples[2] and samples[3] is
	// far larger than maxGap — span+count alone is not enough (issue #15 /
	// issue #7's "gap-aware window coverage").
	samples := []metrics.Sample{
		sampleAt(0, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond),
		sampleAt(1, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond),
		sampleAt(2, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond),
		sampleAt(10, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond), // 8-minute gap
	}
	if windowFull(samples, 5*time.Minute, time.Minute) {
		t.Error("windowFull = true, want false — an 8-minute gap exceeds a 1-minute maxGap")
	}
	if !windowFull(samples, 5*time.Minute, 10*time.Minute) {
		t.Error("windowFull = false, want true — a 10-minute maxGap tolerates the 8-minute gap")
	}
}

// --- issue #9 (reopened): exclude inactive-direction zero latency ---

func TestComputeBaseline_excludesInactiveDirectionZero(t *testing.T) {
	// Every sample only has read activity; WriteLatAvg here stands in for
	// metrics.computeSample's zero-value-when-idle field, not a real 0ns
	// observation (issue #9). Pooling it anyway would drag the baseline
	// toward zero.
	samples := []metrics.Sample{
		sampleAt(0, 100, 0, 0, 0, 0, 10*time.Millisecond, 0),
		sampleAt(1, 100, 0, 0, 0, 0, 10*time.Millisecond, 0),
		sampleAt(2, 100, 0, 0, 0, 0, 10*time.Millisecond, 0),
	}
	got, ok := computeBaseline(samples)
	if !ok {
		t.Fatal("computeBaseline ok = false, want true — the read direction has qualifying samples")
	}
	if got.P95 != 10*time.Millisecond {
		t.Errorf("Baseline.P95 = %v, want 10ms — the idle write direction's phantom zero must not pull it down", got.P95)
	}
}

func TestComputeBaseline_allIdleReturnsNotOk(t *testing.T) {
	// Nonzero latency fields but zero ops in both directions — exactly the
	// shape a totally idle interval produces once the zero-value fields are
	// excluded. Establishing a "baseline" from this would be a bogus 0.
	samples := []metrics.Sample{
		sampleAt(0, 0, 0, 0, 0, 0, 5*time.Millisecond, 5*time.Millisecond),
	}
	_, ok := computeBaseline(samples)
	if ok {
		t.Error("computeBaseline ok = true, want false — no sample had ops>0 in either direction")
	}
}

func TestReadWriteLatencies_excludeInactiveSamples(t *testing.T) {
	samples := []metrics.Sample{
		sampleAt(0, 100, 0, 0, 0, 0, 5*time.Millisecond, 0),
		sampleAt(1, 0, 50, 0, 0, 0, 0, 7*time.Millisecond),
	}
	r := readLatencies(samples)
	if len(r) != 1 || r[0] != 5*time.Millisecond {
		t.Errorf("readLatencies = %v, want [5ms] (only the read-active sample)", r)
	}
	w := writeLatencies(samples)
	if len(w) != 1 || w[0] != 7*time.Millisecond {
		t.Errorf("writeLatencies = %v, want [7ms] (only the write-active sample)", w)
	}
}

// --- issue #7 (reopened): buffer slack + gap-aware window coverage ---

func TestWindowFull_toleratesJitterWithinMaxGap(t *testing.T) {
	// Real ticks are never perfectly evenly spaced. Gaps of 55s/65s around
	// a nominal 60s cadence, all comfortably under a 90s maxGap, must still
	// read as a full, sustained window — jitter is not the same failure
	// mode as a genuine outage.
	base := epoch
	offsets := []time.Duration{0, 55 * time.Second, 120 * time.Second, 175 * time.Second, 240 * time.Second, 300 * time.Second}
	samples := make([]metrics.Sample, 0, len(offsets))
	for _, off := range offsets {
		samples = append(samples, metrics.Sample{DiskID: "disk1", Timestamp: base.Add(off), Source: metrics.SourceGuest})
	}
	if !windowFull(samples, 5*time.Minute, 90*time.Second) {
		t.Error("windowFull = false, want true — jitter within maxGap must not read as a coverage gap")
	}
}

func TestWindowFull_sparseSpanButBelowMinSamplesIsNotFull(t *testing.T) {
	// Only 2 samples, but their span alone already covers the window —
	// minSamplesForWindow's floor must still block this: a window this
	// sparse carries almost no real information despite "technically"
	// spanning the right amount of wall-clock time.
	samples := []metrics.Sample{
		{DiskID: "disk1", Timestamp: epoch, Source: metrics.SourceGuest},
		{DiskID: "disk1", Timestamp: epoch.Add(5 * time.Minute), Source: metrics.SourceGuest},
	}
	if windowFull(samples, 5*time.Minute, time.Hour) {
		t.Error("windowFull = true, want false — 2 samples must never satisfy the minSamplesForWindow floor regardless of span")
	}
}

func TestAnalyze_recoversAfterLongOutageWithoutPoisoningFromOldHistory(t *testing.T) {
	// The buffer still retains history from before a long collection
	// outage — that alone must not prevent WindowFullUp from becoming
	// true again once enough contiguous, fresh samples have accumulated.
	// This is exactly what trailingWindow + windowFull(upWindow, ...)
	// (rather than windowFull(samples, ...) on the whole buffer) is
	// supposed to guarantee.
	a := NewWindowAnalyzer()

	var samples []metrics.Sample
	// Old segment, minutes 0-2 — clustered together, long before the gap.
	for m := 0; m <= 2; m++ {
		samples = append(samples, sampleAt(m, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond))
	}
	// A long outage follows (implicit: the next sample jumps to minute 100
	// — a ~98-minute gap), then a clean, contiguous recovery: minutes
	// 100-105, exactly spanning the 5-minute ScaleUpWindow with no
	// internal gaps.
	for m := 100; m <= 105; m++ {
		samples = append(samples, sampleAt(m, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond))
	}

	policy := testPolicy
	policy.MaxSampleGap = 2 * time.Minute // tolerates the recovered segment's 1-minute cadence, not the ~98-minute outage
	got := a.Analyze("disk1", samples, policy, specIOPS, asOf(samples))

	if !got.WindowFullUp {
		t.Error("WindowFullUp = false, want true — the trailing slice is a clean, gap-free 5-minute window; old pre-outage history sitting deeper in the buffer must not block it")
	}
}

func TestAnalyze_baselineNotEstablishedFromIdleWindow(t *testing.T) {
	// A full up-window where nothing was ever active in either direction —
	// currentBaseline must decline to establish (issue #9), not freeze a
	// bogus 0 in place forever the way ADR-012's "seeds from the disk's
	// first full window" would otherwise do here.
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		samples = append(samples, sampleAt(m, 0, 0, 0, 0, 0, 3*time.Millisecond, 3*time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.BaselineEstablished {
		t.Error("BaselineEstablished = true, want false — an idle window has nothing honest to seed a baseline from")
	}
}

// --- issue #17: bandwidth-aware scale-down sizing ---

// blockSample builds one down-window sample of a workload doing opsPerSec
// operations of blockSize bytes each, purely for demandP95Down's
// ops-vs-bandwidth comparison — queue/latency/timestamps don't matter for
// these tests.
func blockSample(minute int, opsPerSec float64, blockSize int64) metrics.Sample {
	return sampleAt(minute, opsPerSec, 0, opsPerSec*float64(blockSize), 0, 0, time.Millisecond, time.Millisecond)
}

func TestDemandP95Down_64KiBWorkloadIsBandwidthLimited(t *testing.T) {
	// issue #17's own worked example: 900 ops/s at 64 KiB/op ≈ 59 MB/s,
	// whose IOPS-equivalent (÷32768, ADR-008's own ratio) is 1800 — well
	// above the 900 ops/s figure alone. Sizing from ops/s alone (900)
	// would derive a target whose bandwidth ceiling sits below what this
	// workload already sustains.
	window := make([]metrics.Sample, 6)
	for i := range window {
		window[i] = blockSample(i, 900, 64*1024)
	}
	iopsP95, bwP95, effective, limiting := demandP95Down(window)

	if iopsP95 != 900 {
		t.Errorf("iopsP95 = %v, want 900", iopsP95)
	}
	wantBW := 900.0 * 64 * 1024 / bytesPerIOPS // 1800
	if bwP95 != wantBW {
		t.Errorf("bwP95 = %v, want %v (IOPS-equivalent of 900 ops/s at 64KiB)", bwP95, wantBW)
	}
	if effective != wantBW {
		t.Errorf("effective demand = %v, want %v — bandwidth must determine it here, not the lower ops/s figure", effective, wantBW)
	}
	if limiting != "bandwidth" {
		t.Errorf("limiting resource = %q, want %q", limiting, "bandwidth")
	}
}

func TestDemandP95Down_4KiBWorkloadIsIOPSLimited(t *testing.T) {
	// The opposite shape: small blocks, high ops/s, low bandwidth — IOPS
	// must determine the target, not the much smaller bandwidth-equivalent
	// figure.
	window := make([]metrics.Sample, 6)
	for i := range window {
		window[i] = blockSample(i, 900, 4*1024)
	}
	iopsP95, bwP95, effective, limiting := demandP95Down(window)

	if iopsP95 != 900 {
		t.Errorf("iopsP95 = %v, want 900", iopsP95)
	}
	wantBW := 900.0 * 4 * 1024 / bytesPerIOPS // 112.5
	if bwP95 != wantBW {
		t.Errorf("bwP95 = %v, want %v", bwP95, wantBW)
	}
	if effective != 900 {
		t.Errorf("effective demand = %v, want 900 — IOPS must determine it here", effective)
	}
	if limiting != "iops" {
		t.Errorf("limiting resource = %q, want %q", limiting, "iops")
	}
}

func TestDemandP95Down_alignedPerSampleMaxNotCombinedPercentiles(t *testing.T) {
	// The exact failure mode issue #17 warns against: max(p95(ops),
	// p95(bandwidth)) can silently understate demand, because the two
	// percentiles can be driven by *different* samples — no single moment
	// of the workload the disk actually served. 18 "background" samples
	// have ops==bandwidth-equivalent==100 (nothing to disambiguate); one
	// outlier sample spikes ops to 5000 while bandwidth stays low, and a
	// different outlier sample spikes bandwidth-equivalent to 5000 while
	// ops stays low. Neither individual percentile series (computed
	// independently) ever climbs high enough to surface either spike at
	// p95 — but the properly-aligned per-sample-max series does, because
	// each sample's own max(ops, bandwidth) is 5000 for both outliers.
	window := make([]metrics.Sample, 0, 20)
	for i := 0; i < 18; i++ {
		window = append(window, sampleAt(i, 100, 0, 100*bytesPerIOPS, 0, 0, time.Millisecond, time.Millisecond))
	}
	window = append(window, sampleAt(18, 5000, 0, 50*bytesPerIOPS, 0, 0, time.Millisecond, time.Millisecond)) // ops outlier
	window = append(window, sampleAt(19, 50, 0, 5000*bytesPerIOPS, 0, 0, time.Millisecond, time.Millisecond)) // bandwidth outlier

	iopsP95, bwP95, effective, _ := demandP95Down(window)

	if iopsP95 != 100 {
		t.Fatalf("test setup: iopsP95 = %v, want 100 (the ops outlier must not surface at p95 on its own)", iopsP95)
	}
	if bwP95 != 100 {
		t.Fatalf("test setup: bwP95 = %v, want 100 (the bandwidth outlier must not surface at p95 on its own)", bwP95)
	}
	wrongCombined := iopsP95
	if bwP95 > wrongCombined {
		wrongCombined = bwP95
	}
	if wrongCombined != 100 {
		t.Fatalf("test setup: max(iopsP95, bwP95) = %v, want 100 — the whole point is that this naive combination misses both outliers", wrongCombined)
	}
	if effective != 5000 {
		t.Errorf("effective demand = %v, want 5000 — the aligned per-sample max must surface an outlier that either independent percentile alone misses", effective)
	}
}
