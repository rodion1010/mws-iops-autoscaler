package decider

import (
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
)

var epoch = time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)

var testPolicy = EffectivePolicy{
	MinIOPS:              1000,
	MaxIOPS:              10000,
	ScaleUpUtilization:   0.85,
	ScaleDownUtilization: 0.70,
	CooldownScaleUp:      5 * time.Minute,
	CooldownScaleDown:    30 * time.Minute,
	MinChangeIOPS:        200,
	MaxScaleUpRatio:      2.0,
	MaxScaleDownRatio:    0.5,
	MaxChangesPer24h:     48,
}

var plentyOfHeadroom = VMState{IOPSPool: 100000, SiblingP95IOPS: 0}

func lat(ms int) analyzer.LatencyStat {
	return analyzer.LatencyStat{P95: time.Duration(ms) * time.Millisecond, Kind: analyzer.LatencyWindowedAvg}
}

func TestDecide_resizeInProgressBlocksEitherDirection(t *testing.T) {
	d := NewPolicyDecider(true)
	a := analyzer.Analysis{Saturated: true}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{ResizeInProgress: true}, epoch)
	if got.Action != ActionNoAction || got.Reason != "resize_in_progress" {
		t.Errorf("got %+v, want NoAction/resize_in_progress", got)
	}
	if got.Target != got.Current {
		t.Errorf("Target = %d, want == Current (%d) for a blocked decision", got.Target, got.Current)
	}
}

func TestDecide_vmCapReached(t *testing.T) {
	d := NewPolicyDecider(true)
	a := analyzer.Analysis{
		Saturated: true, QueueGrowthRate: 10, ServedIOPS: 900,
	}
	vm := VMState{IOPSPool: 1000, SiblingP95IOPS: 500} // headroom = 500 <= current
	got := d.Decide("disk1", 1000, a, vm, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionVMCapReached {
		t.Errorf("Action = %v, want ActionVMCapReached", got.Action)
	}
	if got.Reason != "vm_headroom_insufficient" {
		t.Errorf("Reason = %q, want vm_headroom_insufficient", got.Reason)
	}
}

func TestDecide_cooldownScaleUpBlocks(t *testing.T) {
	d := NewPolicyDecider(true)
	a := analyzer.Analysis{Saturated: true, QueueGrowthRate: 10, ServedIOPS: 900}
	state := GuardrailState{LastChangeUp: epoch.Add(-1 * time.Minute)} // 1m ago, cooldown is 5m
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, state, epoch)
	if got.Action != ActionNoAction || got.Reason != "cooldown_scale_up" {
		t.Errorf("got %+v, want NoAction/cooldown_scale_up", got)
	}
}

func TestDecide_cooldownScaleDownBlocks(t *testing.T) {
	d := NewPolicyDecider(true)
	a := analyzer.Analysis{Underutilized: true, IOPSP95Down: 100, EffectiveDemandP95Down: 100}
	state := GuardrailState{LastChangeDown: epoch.Add(-1 * time.Minute)} // cooldown is 30m
	got := d.Decide("disk1", 2000, a, plentyOfHeadroom, testPolicy, state, epoch)
	if got.Action != ActionNoAction || got.Reason != "cooldown_scale_down" {
		t.Errorf("got %+v, want NoAction/cooldown_scale_down", got)
	}
}

func TestDecide_scaleUpOpenRegime(t *testing.T) {
	d := NewPolicyDecider(true)
	// (900 served + 100 growth) / 0.85 rounds up to 1200 IOPS.
	a := analyzer.Analysis{Saturated: true, QueueGrowthRate: 100, QueueGrowthRateValid: true, ServedIOPS: 900}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionScaleUp {
		t.Fatalf("Action = %v, want ActionScaleUp (got %+v)", got.Action, got)
	}
	if got.Target != 1200 {
		t.Errorf("Target = %d, want 1200", got.Target)
	}
	if got.Demand != 1000 {
		t.Errorf("Demand = %v, want 1000 (open regime: served + growth)", got.Demand)
	}
}

func TestDecide_scaleUpClosedRegimeRatioClamped(t *testing.T) {
	d := NewPolicyDecider(true)
	// Stable queue selects the closed regime: latency is three times baseline.
	a := analyzer.Analysis{
		Saturated: true, QueueGrowthRate: 0, ServedIOPS: 900,
		BaselineEstablished: true, Baseline: lat(1),
		ReadLatency: lat(3), WriteLatency: lat(2),
	}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionScaleUp {
		t.Fatalf("Action = %v, want ActionScaleUp (got %+v)", got.Action, got)
	}
	if got.Target != 2000 {
		t.Errorf("Target = %d, want 2000 (clamped by MaxScaleUpRatio)", got.Target)
	}
}

func TestDecide_baselineUnavailableIsDefensiveNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// Defensive case: a hand-built saturated Analysis lacks a baseline.
	a := analyzer.Analysis{Saturated: true, QueueGrowthRate: 0, BaselineEstablished: false}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "baseline_unavailable" {
		t.Errorf("got %+v, want NoAction/baseline_unavailable", got)
	}
}

func TestDecide_targetBelowMinChangeIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// (900 served + 5 growth) / 0.85 rounds to 1100, below min_change.
	a := analyzer.Analysis{Saturated: true, QueueGrowthRate: 5, QueueGrowthRateValid: true, ServedIOPS: 900}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "target_not_above_current" {
		t.Errorf("got %+v, want NoAction/target_not_above_current", got)
	}
	if got.Target != got.Current {
		t.Errorf("Target = %d, want == Current for a suppressed change", got.Target)
	}
}

func TestDecide_budgetExhaustedBlocksOtherwiseValidScaleUp(t *testing.T) {
	d := NewPolicyDecider(true)
	a := analyzer.Analysis{Saturated: true, QueueGrowthRate: 100, QueueGrowthRateValid: true, ServedIOPS: 900}
	state := GuardrailState{ChangesInLast24h: testPolicy.MaxChangesPer24h}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, state, epoch)
	if got.Action != ActionNoAction || got.Reason != "budget_exhausted" {
		t.Errorf("got %+v, want NoAction/budget_exhausted", got)
	}
}

func TestDecide_scaleDown(t *testing.T) {
	d := NewPolicyDecider(true)
	// Demand rounds to 500, then max_scale_down_ratio clamps it to 1000.
	a := analyzer.Analysis{Underutilized: true, IOPSP95Down: 300, EffectiveDemandP95Down: 300}
	got := d.Decide("disk1", 2000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionScaleDown {
		t.Fatalf("Action = %v, want ActionScaleDown (got %+v)", got.Action, got)
	}
	if got.Target != 1000 {
		t.Errorf("Target = %d, want 1000 (floored by MaxScaleDownRatio)", got.Target)
	}
}

func TestDecide_scaleDownNeverBelowMinIOPS(t *testing.T) {
	d := NewPolicyDecider(true)
	// Keep Current high enough for the minimum-change assertion after ratio clamping.
	a := analyzer.Analysis{Underutilized: true, IOPSP95Down: 50, EffectiveDemandP95Down: 50}
	got := d.Decide("disk1", 1500, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionScaleDown {
		t.Fatalf("Action = %v, want ActionScaleDown (got %+v)", got.Action, got)
	}
	if got.Target != testPolicy.MinIOPS {
		t.Errorf("Target = %d, want MinIOPS (%d)", got.Target, testPolicy.MinIOPS)
	}
}

func TestDecide_scaleDownTargetRetainsHeadroomForBothIOPSAndBandwidth(t *testing.T) {
	d := NewPolicyDecider(true)
	// Large blocks make effective demand 1800 IOPS-equivalent rather than 900 served IOPS.
	a := analyzer.Analysis{
		Underutilized:             true,
		IOPSP95Down:               900,
		BandwidthP95DownIOPSEquiv: 1800,
		EffectiveDemandP95Down:    1800,
		LimitingResourceDown:      "bandwidth",
	}
	got := d.Decide("disk1", 5000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionScaleDown {
		t.Fatalf("Action = %v, want ActionScaleDown (got %+v)", got.Action, got)
	}
	// 1800 / 0.70 rounds up to 2600 IOPS.
	if got.Target != 2600 {
		t.Errorf("Target = %d, want 2600 (sized from EffectiveDemandP95Down=1800, not IOPSP95Down=900)", got.Target)
	}
	if got.Demand != 1800 {
		t.Errorf("Demand = %v, want 1800 (EffectiveDemandP95Down, the bandwidth-dominated figure)", got.Demand)
	}
	// Sizing from served IOPS alone would incorrectly choose 1300.
	const undersizedFromOpsAlone = 1300
	if got.Target <= undersizedFromOpsAlone {
		t.Errorf("Target = %d does not exceed %d — bandwidth demand was not retained", got.Target, undersizedFromOpsAlone)
	}
}

func TestDecide_scaleDownDisabledNeverWrites(t *testing.T) {
	// Process capability disables scale-down independently of policy.
	d := NewPolicyDecider(false)
	a := analyzer.Analysis{Underutilized: true, IOPSP95Down: 300, EffectiveDemandP95Down: 300}
	got := d.Decide("disk1", 2000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction {
		t.Fatalf("Action = %v, want ActionNoAction (got %+v)", got.Action, got)
	}
	if got.Reason != "scale_down_disabled" {
		t.Errorf("Reason = %q, want scale_down_disabled", got.Reason)
	}
	if got.Target != got.Current {
		t.Errorf("Target = %d, want == Current (%d) — no candidate target when the direction is disabled", got.Target, got.Current)
	}
	if got.Demand != 300 {
		t.Errorf("Demand = %v, want 300 (candidate recommendation still surfaced for observability)", got.Demand)
	}
}

func TestDecide_scaleDownDisabledIgnoresCooldownAndBudget(t *testing.T) {
	// The capability gate precedes cooldown and budget checks.
	d := NewPolicyDecider(false)
	a := analyzer.Analysis{Underutilized: true, IOPSP95Down: 300, EffectiveDemandP95Down: 300}
	state := GuardrailState{} // no cooldown active, full budget
	got := d.Decide("disk1", 2000, a, plentyOfHeadroom, testPolicy, state, epoch)
	if got.Action != ActionNoAction || got.Reason != "scale_down_disabled" {
		t.Errorf("got %+v, want NoAction/scale_down_disabled regardless of cooldown/budget state", got)
	}
}

func TestDecide_scaleUpUnaffectedByScaleDownGate(t *testing.T) {
	d := NewPolicyDecider(false)
	a := analyzer.Analysis{Saturated: true, QueueGrowthRate: 100, QueueGrowthRateValid: true, ServedIOPS: 900}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionScaleUp {
		t.Errorf("Action = %v, want ActionScaleUp — the scale-down gate must not affect scale-up", got.Action)
	}
}

func TestDecide_neitherSignalIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// Fresh, full, quiet evidence is a real no-signal outcome.
	a := analyzer.Analysis{Saturated: false, Underutilized: false, HasData: true, Fresh: true, WindowFullUp: true, WindowFullDown: true}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "no_signal" {
		t.Errorf("got %+v, want NoAction/no_signal", got)
	}
}

func TestDecide_noDataIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// HasData=false represents true cold start.
	a := analyzer.Analysis{Saturated: false, Underutilized: false, HasData: false, Fresh: false}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "no_data" {
		t.Errorf("got %+v, want NoAction/no_data", got)
	}
}

func TestDecide_staleMetricsIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// HasData=true with Fresh=false represents previously available data becoming stale.
	a := analyzer.Analysis{Saturated: false, Underutilized: false, HasData: true, Fresh: false, WindowFullUp: true, WindowFullDown: true}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "metrics_stale" {
		t.Errorf("got %+v, want NoAction/metrics_stale", got)
	}
}

func TestDecide_windowNotFullIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// Fresh but incomplete windows must report cold start.
	a := analyzer.Analysis{Saturated: false, Underutilized: false, HasData: true, Fresh: true, WindowFullUp: false, WindowFullDown: false}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "window_not_full" {
		t.Errorf("got %+v, want NoAction/window_not_full", got)
	}
}

func TestDecide_vmHeadroomAndBudgetRemainingAlwaysSet(t *testing.T) {
	d := NewPolicyDecider(true)
	vm := VMState{IOPSPool: 6000, SiblingP95IOPS: 1500}
	state := GuardrailState{ResizeInProgress: true, ChangesInLast24h: 10}
	got := d.Decide("disk1", 1000, analyzer.Analysis{}, vm, testPolicy, state, epoch)
	if got.VMHeadroomIOPS != 4500 {
		t.Errorf("VMHeadroomIOPS = %d, want 4500 (6000 - 1500), even on a resize_in_progress NoAction", got.VMHeadroomIOPS)
	}
	if want := testPolicy.MaxChangesPer24h - 10; got.BudgetRemaining != want {
		t.Errorf("BudgetRemaining = %d, want %d", got.BudgetRemaining, want)
	}
}

func TestDecide_diskIDAndCurrentSetEvenOnNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	got := d.Decide("disk-xyz", 3000, analyzer.Analysis{}, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.DiskID != "disk-xyz" || got.Current != 3000 {
		t.Errorf("got DiskID=%q Current=%d, want disk-xyz/3000", got.DiskID, got.Current)
	}
}


func TestGuardrails_ceilingNeverExceededEvenWhenNotOnGrid(t *testing.T) {
	// A fractional VM ceiling must floor to the billing grid, never round above the bound.
	target, changed := guardrails(10000 /* raw */, 3000 /* current */, 4523, /* ceiling */
		1000 /* minIOPS */, 2.0 /* ratio */, 200 /* minChangeIOPS */)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if target != 4500 {
		t.Errorf("target = %d, want 4500 (floored to the 100-grid under the 4523 ceiling, never 4600)", target)
	}
	if target > 4523 {
		t.Fatalf("target = %d exceeds the ceiling 4523", target)
	}
}

func TestGuardrails_minChangeSuppressesSmallDelta(t *testing.T) {
	target, changed := guardrails(1050, 1000, 10000, 1000, 2.0, 200)
	if changed {
		t.Errorf("changed = true, target = %d — want suppressed (delta 100 < minChangeIOPS 200)", target)
	}
	if target != 1000 {
		t.Errorf("target = %d, want current (1000) when suppressed", target)
	}
}

func TestGuardrails_neverBelowMinIOPS(t *testing.T) {
	target, changed := guardrails(1, 1200, 10000, 1000, 0.5, 200)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if target != 1000 {
		t.Errorf("target = %d, want 1000 (minIOPS floor)", target)
	}
}

func TestHeadroomCeiling_takesTightestBound(t *testing.T) {
	p := EffectivePolicy{MaxIOPS: 8000}
	cases := []struct {
		name string
		vm   VMState
		want int64
	}{
		{"platform max binds", VMState{IOPSPool: 100000, SiblingP95IOPS: 0}, 8000}, // policy max (8000) < platform max (10000)
		{"vm headroom binds", VMState{IOPSPool: 5000, SiblingP95IOPS: 3000}, 2000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := headroomCeiling(tc.vm, p); got != tc.want {
				t.Errorf("headroomCeiling() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDemand_openRegimeIgnoresLatency(t *testing.T) {
	a := analyzer.Analysis{QueueGrowthRate: 50, QueueGrowthRateValid: true, ServedIOPS: 800, BaselineEstablished: false}
	got, ok := demand(a)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != 850 {
		t.Errorf("demand = %v, want 850 (served + growth)", got)
	}
}

func TestDemand_closedRegimeUsesWorstOfReadWrite(t *testing.T) {
	a := analyzer.Analysis{
		QueueGrowthRate: 0, ServedIOPS: 1000, BaselineEstablished: true,
		Baseline: lat(2), ReadLatency: lat(4), WriteLatency: lat(10),
	}
	got, ok := demand(a)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if want := 1000.0 * (10.0 / 2.0); got != want {
		t.Errorf("demand = %v, want %v (served × worst(read,write)/baseline)", got, want)
	}
}

func TestDemand_closedRegimeUnavailableWithoutBaseline(t *testing.T) {
	a := analyzer.Analysis{QueueGrowthRate: 0, BaselineEstablished: false}
	if _, ok := demand(a); ok {
		t.Error("ok = true, want false — no baseline and no growth means no demand estimate")
	}
}

	// Byte-denominated queue growth cannot be added to served IOPS.
func TestDemand_invalidQueueGrowthRateNeverSums(t *testing.T) {
	a := analyzer.Analysis{
		QueueGrowthRate:      1_000_000, // a bytes/s-scale number, not ops/s
		QueueGrowthRateValid: false,
		ServedIOPS:           800,
		BaselineEstablished:  false,
	}
	_, ok := demand(a)
	if ok {
		t.Error("ok = true, want false — an invalid growth rate must never be summed with ServedIOPS regardless of its magnitude")
	}
}

	// Invalid queue units fall back to latency demand rather than max-clamp.
func TestDecide_invalidQueueGrowthRateFallsBackNotMaxClamp(t *testing.T) {
	d := NewPolicyDecider(true)
	a := analyzer.Analysis{
		Saturated:            true,
		QueueGrowthRate:      1_000_000,
		QueueGrowthRateValid: false,
		ServedIOPS:           800,
		BaselineEstablished:  false,
	}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "baseline_unavailable" {
		t.Errorf("got %+v, want NoAction/baseline_unavailable — an invalid growth rate must never drive a scale-up, however large its raw number", got)
	}
}
