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
}

var epoch = time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)

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
	got := a.Analyze("disk1", nil, testPolicy, specIOPS)
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
	got := a.Analyze("disk1", samples, testPolicy, specIOPS)
	if got.WindowFullUp {
		t.Error("WindowFullUp = true, want false (span is 3m, ScaleUpWindow is 5m)")
	}
	if got.Saturated {
		t.Error("Saturated = true, want false — a partial window must never act (NFR-9)")
	}
}

func TestAnalyze_exactlyFullWindowIsFull(t *testing.T) {
	a := NewWindowAnalyzer()
	got := a.Analyze("disk1", quietUpWindow(), testPolicy, specIOPS)
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
	got := a.Analyze("disk1", samples, testPolicy, specIOPS)
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
	got := a.Analyze("disk1", samples, testPolicy, specIOPS)
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
	if got := a.Analyze("disk1", quietUpWindow(), testPolicy, specIOPS); got.Saturated {
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
	got := a.Analyze("disk1", samples, testPolicy, specIOPS)
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
	a.Analyze("disk1", quietUpWindow(), testPolicy, specIOPS)

	// Cycle 2: served pinned at the provisioned limit (990/1000, within
	// throttleTolerance), queue > 0 but below QueueDepthThreshold (1) so
	// the queue-depth trigger itself doesn't independently fire, latency
	// flat at baseline.
	samples := make([]metrics.Sample, 0, 6)
	for m := 100; m <= 105; m++ {
		samples = append(samples, sampleAt(m, 990, 0, 0, 0, 0.5, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, specIOPS)
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
	got := a.Analyze("disk1", samples, testPolicy, specIOPS)
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
	got := a.Analyze("disk1", samples, testPolicy, specIOPS)
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
	first := a.Analyze("disk1", saturated, testPolicy, specIOPS)
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
	second := a.Analyze("disk1", stillSaturated, testPolicy, specIOPS)
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
	third := a.Analyze("disk1", quiet, testPolicy, specIOPS)
	if third.Saturated {
		t.Fatal("test setup: cycle 3 must not be saturated")
	}
	if third.Baseline.P95 != baselineAfterCycle1 {
		t.Errorf("cycle 3's own reported Baseline.P95 = %v, want %v (the update it triggers only affects the next lookup)",
			third.Baseline.P95, baselineAfterCycle1)
	}

	// Cycle 4: the freeze must have lifted after cycle 3's quiet update.
	fourth := a.Analyze("disk1", quiet, testPolicy, specIOPS)
	if fourth.Baseline.P95 == baselineAfterCycle1 {
		t.Error("Baseline.P95 never updated after saturation cleared — the freeze must lift, not persist forever")
	}
}

func TestAnalyze_underutilizedOnSustainedQuiet(t *testing.T) {
	a := NewWindowAnalyzer()
	got := a.Analyze("disk1", quietDownWindow(), testPolicy, specIOPS)
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
	got := a.Analyze("disk1", samples, testPolicy, specIOPS)
	if got.Underutilized {
		t.Error("Underutilized = true, want false — one non-quiet sample in the down-window must break sustain")
	}
}

func TestAnalyze_nonPositiveSpecIOPSIsDefensive(t *testing.T) {
	a := NewWindowAnalyzer()
	samples := make([]metrics.Sample, 0, 6)
	for m := 0; m <= 5; m++ {
		samples = append(samples, sampleAt(m, 1000, 0, 0, 0, 5, time.Millisecond, time.Millisecond))
	}
	got := a.Analyze("disk1", samples, testPolicy, 0)
	if got.IOPSUtilization != 0 || got.BandwidthUtilization != 0 {
		t.Errorf("utilization with specIOPS<=0 = (%v, %v), want (0, 0)", got.IOPSUtilization, got.BandwidthUtilization)
	}
	if got.ThrottleDetected {
		t.Error("ThrottleDetected = true with specIOPS<=0, want false — no provisioned limit to compare against")
	}
}
