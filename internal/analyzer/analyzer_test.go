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
	MaxSampleAge: time.Hour,
	MaxSampleGap: time.Hour,
}

var epoch = time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)

func asOf(samples []metrics.Sample) time.Time {
	return samples[len(samples)-1].Timestamp
}

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

func quietUpWindow() []metrics.Sample {
	out := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		out = append(out, sampleAt(m, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond))
	}
	return out
}

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
	// Utilization reaches 100%, but no saturation signal fires.
		samples = append(samples, sampleAt(m, 1000, 0, 0, 0, 0, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if got.IOPSUtilization < testPolicy.UtilizationThreshold {
		t.Fatalf("test setup: IOPSUtilization = %v, want >= threshold", got.IOPSUtilization)
	}
	if got.Saturated {
		t.Error("Saturated = true, want false — a disk at 100% utilization with an empty queue and flat latency is efficient (docs/design.md §4.4)")
	}
}

func TestAnalyze_queueDepthAloneTriggers(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
	// Bandwidth alone opens the resource gate for this large-block workload.
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

	// Cycle 1 establishes the quiet latency baseline.
	up := quietUpWindow()
	if got := a.Analyze("disk1", up, testPolicy, specIOPS, asOf(up)); got.Saturated {
		t.Fatal("test setup: cycle 1 must not be saturated")
	} else if !got.BaselineEstablished {
		t.Fatal("test setup: baseline must establish from the first full window")
	}

	// Cycle 2 is bandwidth-saturated only through elevated latency.
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

	// Cycle 1 establishes the quiet latency baseline.
	up := quietUpWindow()
	a.Analyze("disk1", up, testPolicy, specIOPS, asOf(up))

	// Cycle 2 is throttled near the provisioned limit with a non-empty queue.
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
		t.Error("BaselineEstablished = false, want true — establishment must not be blocked by saturation (docs/design.md §4.4)")
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

	// Saturation freezes the lower-latency baseline update.
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

	// A quiet cycle updates the stored baseline for subsequent analysis.
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

	// The quiet update must unfreeze the baseline.
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

func TestAnalyze_notUnderutilizedWhenBandwidthElevated(t *testing.T) {
	a := NewWindowAnalyzer()
	// 1000 IOPS at 32 KiB per operation equals the provisioned bandwidth ceiling.
	const nearCeilingBytes = 0.99 * 1000 * 32768
	samples := make([]metrics.Sample, 0, 31)
	for m := 0; m <= 30; m++ {
		samples = append(samples, sampleAt(m, 100, 0, nearCeilingBytes, 0, 0, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS, asOf(samples))
	if !got.WindowFullDown {
		t.Fatal("test setup: expected WindowFullDown = true (span is exactly 30m)")
	}
	if got.Underutilized {
		t.Error("Underutilized = true, want false — bandwidth utilization near its ceiling must block the quiet verdict even though ops/s, queue, and latency all look idle")
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
	// Queue rises from 0 to 5 over 300 seconds.
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
		t.Error("QueueGrowthRateValid = false, want true — guest samples are always ops-denominated (docs/design.md §4.2)")
	}
}


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
	// Four samples span three of the required five minutes.
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


func TestAnalyze_staleSamplesNeverSaturate(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
	// This shape would be saturated if its evidence were fresh.
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


func TestWindowFull_gapBreaksFullness(t *testing.T) {
	// Count and total span pass, but the internal outage invalidates the window.
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


func TestComputeBaseline_excludesInactiveDirectionZero(t *testing.T) {
	// Only reads are active; write latency is a phantom zero value.
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
	// Nonzero latency fields with zero operations must not establish a baseline.
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


func TestWindowFull_toleratesJitterWithinMaxGap(t *testing.T) {
	// Scheduler jitter alternates 55s and 65s around a one-minute cadence.
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
	// Two endpoints span the window but do not meet the minimum sample count.
	samples := []metrics.Sample{
		{DiskID: "disk1", Timestamp: epoch, Source: metrics.SourceGuest},
		{DiskID: "disk1", Timestamp: epoch.Add(5 * time.Minute), Source: metrics.SourceGuest},
	}
	if windowFull(samples, 5*time.Minute, time.Hour) {
		t.Error("windowFull = true, want false — 2 samples must never satisfy the minSamplesForWindow floor regardless of span")
	}
}

func TestAnalyze_recoversAfterLongOutageWithoutPoisoningFromOldHistory(t *testing.T) {
	// Retained pre-outage history must not bridge the recovered window.
	a := NewWindowAnalyzer()

	var samples []metrics.Sample
	// Pre-outage samples.
	for m := 0; m <= 2; m++ {
		samples = append(samples, sampleAt(m, 100, 0, 0, 0, 0, time.Millisecond, time.Millisecond))
	}
	// Sampling resumes at minute 100 after a long outage.
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


func TestTrailingWindow_jitterNeverPreventsFullSpan(t *testing.T) {
	// Thirty-one samples include small scheduler jitter around one-minute cadence.
	base := epoch
	jitter := []time.Duration{0, 3, -2, 4, -1, 2, 0, -3, 1, 3, -2, 2, 0, 1, -1, 4, -3, 2, 1, -2, 0, 3, -1, 2, -3, 1, 0, 4, -2, 1, 0}
	samples := make([]metrics.Sample, len(jitter))
	for i, j := range jitter {
		samples[i] = metrics.Sample{
			DiskID:    "disk1",
			Timestamp: base.Add(time.Duration(i)*time.Minute + j*time.Second),
			Source:    metrics.SourceGuest,
		}
	}

	window := 5 * time.Minute
	got := trailingWindow(samples, window)

	newest := samples[len(samples)-1].Timestamp
	span := newest.Sub(got[0].Timestamp)
	if span < window {
		t.Fatalf("trailingWindow's own span = %v, want >= %v (window) — jitter must not prevent this", span, window)
	}
	if !windowFull(got, window, 90*time.Second) {
		t.Error("windowFull(trailingWindow(...)) = false, want true — jittered-but-continuous samples spanning well over 5 minutes must read as full")
	}
}

func TestTrailingWindow_doesNotReachAcrossAGapItDoesNotNeedTo(t *testing.T) {
	samples := []metrics.Sample{
		{DiskID: "disk1", Timestamp: epoch, Source: metrics.SourceGuest}, // stale, far pre-outage
	}
	for m := 100; m <= 105; m++ {
		samples = append(samples, sampleAt(m, 0, 0, 0, 0, 0, 0, 0))
	}

	got := trailingWindow(samples, 5*time.Minute)

	if len(got) != 6 {
		t.Fatalf("trailingWindow returned %d samples, want exactly the 6-sample recovered segment (the stale predecessor must not be pulled in)", len(got))
	}
	if got[0].Timestamp != samples[1].Timestamp {
		t.Errorf("trailingWindow's first sample = %v, want the recovered segment's own first sample %v", got[0].Timestamp, samples[1].Timestamp)
	}
}

func TestAnalyze_baselineNotEstablishedFromIdleWindow(t *testing.T) {
	// A full window with no active direction has no baseline data.
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


func blockSample(minute int, opsPerSec float64, blockSize int64) metrics.Sample {
	return sampleAt(minute, opsPerSec, 0, opsPerSec*float64(blockSize), 0, 0, time.Millisecond, time.Millisecond)
}

func TestDemandP95Down_64KiBWorkloadIsBandwidthLimited(t *testing.T) {
	// 900 ops/s at 64 KiB each is about 59 MB/s, equivalent to 1800 provisioned IOPS.
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
	// Small blocks make IOPS, not bandwidth, the limiting resource.
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
	// Per-sample maxima must precede the percentile because independent p95s occur at different times.
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
