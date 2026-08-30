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

// plentyOfHeadroom leaves the VM pool nowhere near binding.
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
	// demand = served + growth = 900 + 100 = 1000; raw = 1000/0.85 = 1176.47 -> 1200.
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
	// closed regime: growth below threshold, latency 3x baseline.
	// demand = served(900) * (3ms/1ms) = 2700; raw = 2700/0.85 = 3176.47 -> 3200,
	// but MaxScaleUpRatio(2.0) caps it at current(1000)*2 = 2000.
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
	// Hand-built Analysis violating analyzer's real invariant (Saturated
	// without BaselineEstablished) — exercises Decide's defensive guard
	// directly, since the real pipeline never produces this combination.
	a := analyzer.Analysis{Saturated: true, QueueGrowthRate: 0, BaselineEstablished: false}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "baseline_unavailable" {
		t.Errorf("got %+v, want NoAction/baseline_unavailable", got)
	}
}

func TestDecide_targetBelowMinChangeIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// demand = 900 + 5 = 905; raw = 905/0.85 = 1064.7 -> 1100; delta from
	// current(1000) is only 100, below MinChangeIOPS(200).
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
	// raw = 300/0.70 = 428.6 -> 500, but MaxScaleDownRatio(0.5) floors it
	// at current(2000)*0.5 = 1000.
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
	// current far enough above MinIOPS that the floor-driven delta still
	// clears MinChangeIOPS (200) — otherwise the change is suppressed
	// before the MinIOPS floor is even the interesting part of the story.
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
	// issue #17's own worked example: 900 ops/s at 64 KiB/op is ~59 MB/s,
	// whose IOPS-equivalent (÷32768 using the ratio in docs/design.md §4.4) is 1800 — well
	// above the 900 ops/s figure alone. A target sized from IOPSP95Down
	// alone would undersize the disk's actual bandwidth need; Decide must
	// size from EffectiveDemandP95Down (the aligned per-sample max)
	// instead, so the target's IOPS-equivalent ceiling still covers the
	// observed bandwidth, not just the observed op count.
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
	// raw = 1800/0.70 = 2571.4 -> ceiled to 2600 on the 100-grid.
	if got.Target != 2600 {
		t.Errorf("Target = %d, want 2600 (sized from EffectiveDemandP95Down=1800, not IOPSP95Down=900)", got.Target)
	}
	if got.Demand != 1800 {
		t.Errorf("Demand = %v, want 1800 (EffectiveDemandP95Down, the bandwidth-dominated figure)", got.Demand)
	}
	// Had Decide sized from IOPSP95Down alone, raw = 900/0.70 = 1285.7 ->
	// 1300 — well below the actual bandwidth need. Assert the real target
	// clears that undersized figure, proving bandwidth (not ops alone)
	// drove the result.
	const undersizedFromOpsAlone = 1300
	if got.Target <= undersizedFromOpsAlone {
		t.Errorf("Target = %d does not exceed %d — bandwidth demand was not retained", got.Target, undersizedFromOpsAlone)
	}
}

func TestDecide_scaleDownDisabledNeverWrites(t *testing.T) {
	// issue #14: Phase 1 is scale-up only. With allowScaleDown=false, a
	// quiet full down-window must never reach a write path, checked before
	// cooldown/guardrails, not merely clamped by them.
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
	// The gate must fire even when cooldown/budget would otherwise have
	// allowed the change — it is not implemented as "just another
	// guardrail" that a sufficiently permissive state could bypass.
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
	// Fresh and both windows full — a genuinely quiet, trustworthy disk
	// with nothing to do, distinct from the metrics_stale/window_not_full
	// cases below (issue #15).
	a := analyzer.Analysis{Saturated: false, Underutilized: false, HasData: true, Fresh: true, WindowFullUp: true, WindowFullDown: true}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "no_signal" {
		t.Errorf("got %+v, want NoAction/no_signal", got)
	}
}

func TestDecide_noDataIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// HasData=false (true cold start — no sample has ever arrived for this
	// disk) must be reported distinctly from metrics_stale (had data, now
	// too old) — different operational situations, different fixes
	// (pre-merge review). Checked ahead of Fresh, which is also false
	// here (zero value) — HasData must win, not get shadowed by it.
	a := analyzer.Analysis{Saturated: false, Underutilized: false, HasData: false, Fresh: false}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "no_data" {
		t.Errorf("got %+v, want NoAction/no_data", got)
	}
}

func TestDecide_staleMetricsIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// HasData=true (data arrived before) but Fresh=false (it's since gone
	// stale) must be reported distinctly from both no_data and a plain
	// "no_signal" — this is the case an operator needs to be able to tell
	// apart from a disk that's simply idle (issue #15).
	a := analyzer.Analysis{Saturated: false, Underutilized: false, HasData: true, Fresh: false, WindowFullUp: true, WindowFullDown: true}
	got := d.Decide("disk1", 1000, a, plentyOfHeadroom, testPolicy, GuardrailState{}, epoch)
	if got.Action != ActionNoAction || got.Reason != "metrics_stale" {
		t.Errorf("got %+v, want NoAction/metrics_stale", got)
	}
}

func TestDecide_windowNotFullIsNoAction(t *testing.T) {
	d := NewPolicyDecider(true)
	// Fresh but cold-start (neither window has enough history yet) must
	// also be distinct from both metrics_stale and a genuine no_signal.
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

// --- guardrails() unit tests ---

func TestGuardrails_ceilingNeverExceededEvenWhenNotOnGrid(t *testing.T) {
	// Regression test: ceiling (e.g. from a fractional sibling p95) is not
	// a 100-multiple. The clamp must floor here, not round back up past it.
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

// TestDemand_invalidQueueGrowthRateNeverSums is issue #18's core scenario:
// a large raw QueueGrowthRate (as a future platform-sourced bytes/s
// reading would be) must never reach the open-workload sum just because
// its number happens to be large — only QueueGrowthRateValid decides that,
// never the magnitude. Falls through to the closed-workload regime, and
// with no baseline established here, that means no estimate at all —
// never a bytes-inflated demand that guardrails would clamp to max.
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

// TestDecide_invalidQueueGrowthRateFallsBackNotMaxClamp is the same
// scenario through the full Decide path: a saturated disk with an invalid,
// large QueueGrowthRate and no baseline must end in NoAction/
// baseline_unavailable, never a scale-up target driven by the bogus sum
// (which guardrails would otherwise clamp to the VM/policy ceiling).
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
