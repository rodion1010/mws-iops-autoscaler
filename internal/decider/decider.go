package decider

import (
	"math"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
)

// platformMaxIOPS is the disk-level ceiling the platform itself enforces
// (CLAUDE.md "IOPS limits") — always part of the clamp regardless of what
// policy.limits.max_iops or VM headroom allow.
const platformMaxIOPS = 10000

// billingStepIOPS mirrors internal/config.BillingStepIOPS — duplicated
// rather than imported to keep this package decoupled from internal/config
// (design.md §3.3), the same way analyzer.EvaluationPolicy avoids it.
const billingStepIOPS = 100

// queueGrowthThreshold is the boundary between "open workload, queue
// growing" and "closed workload, queue stable" (design.md §4.5), in
// QueueDepth units per second. No exact value is pinned by any doc; a
// small positive epsilon (rather than a strict >0) avoids misclassifying a
// genuinely stable queue as "growing" on measurement noise alone.
// Calibration-pending per design.md §8.5, same treatment as
// internal/analyzer's constants.
const queueGrowthThreshold = 0.01

// PolicyDecider implements Decider. It holds no per-decision state — every
// input Decide needs (current value, cooldown/budget state, wall clock) is
// an explicit parameter, so it is safe to share and trivial to test. The
// one thing it does hold is allowScaleDown: a build-time capability, not a
// per-call policy value, deliberately (issue #14) — Phase 1 is scale-up
// only (PRD §6), and that must not depend on any user-tunable threshold
// happening to never trigger scale-down. Phase 3 flips exactly this one
// constructor argument; nothing else about Decide changes.
type PolicyDecider struct {
	allowScaleDown bool
}

// NewPolicyDecider is the single, reviewable point where bidirectional
// actuation gets enabled (issue #14's "one explicit, reviewable enablement
// point"). Phase 1 callers must pass false.
func NewPolicyDecider(allowScaleDown bool) *PolicyDecider {
	return &PolicyDecider{allowScaleDown: allowScaleDown}
}

func (pd *PolicyDecider) Decide(diskID string, current int64, a analyzer.Analysis, vm VMState, p EffectivePolicy, state GuardrailState, now time.Time) Decision {
	base := Decision{
		DiskID:  diskID,
		Current: current,
		Target:  current,
		Inputs:  a,
		// VM-level (§4.6), independent of this disk's own policy.limits —
		// distinct from ceiling below, which folds policy/platform bounds
		// in too. BudgetRemaining is set on every path, including
		// resize_in_progress: it describes the disk's standing budget, not
		// something this cycle's action consumed.
		VMHeadroomIOPS:  vm.IOPSPool - int64(vm.SiblingP95IOPS),
		BudgetRemaining: p.MaxChangesPer24h - state.ChangesInLast24h,
	}

	if state.ResizeInProgress {
		return withReason(base, ActionNoAction, "resize_in_progress")
	}

	ceiling := headroomCeiling(vm, p)

	switch {
	case a.Saturated:
		if ceiling <= current {
			return withReason(base, ActionVMCapReached, "vm_headroom_insufficient")
		}
		if now.Sub(state.LastChangeUp) < p.CooldownScaleUp {
			return withReason(base, ActionNoAction, "cooldown_scale_up")
		}
		d, ok := demand(a)
		if !ok {
			return withReason(base, ActionNoAction, "baseline_unavailable")
		}
		base.Demand = d
		raw := d / p.ScaleUpUtilization
		target, changed := guardrails(raw, current, ceiling, p.MinIOPS, p.MaxScaleUpRatio, p.MinChangeIOPS)
		if !changed || target <= current {
			return withReason(base, ActionNoAction, "target_not_above_current")
		}
		if state.ChangesInLast24h >= p.MaxChangesPer24h {
			return withReason(base, ActionNoAction, "budget_exhausted")
		}
		base.Target = target
		return withReason(base, ActionScaleUp, "saturated")

	case a.Underutilized:
		// Checked first, before cooldown/guardrails: with the shipped Phase
		// 1 configuration, a quiet full down-window must never reach any
		// write path, full stop — not "reach it but then get clamped by
		// something else." The candidate demand is still computed and
		// surfaced (issue #14: "optionally retaining the candidate
		// recommendation for observability") so the journal/metrics show
		// what *would* have happened, without it ever becoming a target.
		base.Demand = a.IOPSP95Down
		if !pd.allowScaleDown {
			return withReason(base, ActionNoAction, "scale_down_disabled")
		}
		if now.Sub(state.LastChangeDown) < p.CooldownScaleDown {
			return withReason(base, ActionNoAction, "cooldown_scale_down")
		}
		raw := a.IOPSP95Down / p.ScaleDownUtilization
		target, changed := guardrails(raw, current, ceiling, p.MinIOPS, p.MaxScaleDownRatio, p.MinChangeIOPS)
		if !changed || target >= current {
			return withReason(base, ActionNoAction, "target_not_below_current")
		}
		if state.ChangesInLast24h >= p.MaxChangesPer24h {
			return withReason(base, ActionNoAction, "budget_exhausted")
		}
		base.Target = target
		return withReason(base, ActionScaleDown, "underutilized")

	default:
		return withReason(base, ActionNoAction, "no_signal")
	}
}

func withReason(d Decision, action Action, reason string) Decision {
	d.Action = action
	d.Reason = reason
	return d
}

// headroomCeiling is the disk-level ceiling from every source that bounds
// it (design.md §4.6, NFR-5): the platform's own max, the user's
// policy.limits.max_iops, and the VM's remaining aggregate pool.
func headroomCeiling(vm VMState, p EffectivePolicy) int64 {
	ceiling := int64(platformMaxIOPS)
	if p.MaxIOPS < ceiling {
		ceiling = p.MaxIOPS
	}
	headroom := int64(float64(vm.IOPSPool) - vm.SiblingP95IOPS)
	if headroom < ceiling {
		ceiling = headroom
	}
	return ceiling
}

// demand implements design.md §4.5's two regimes. The closed-regime
// !a.BaselineEstablished branch is defensive, not reachable through the
// real pipeline: analyzer.Analyze always sets BaselineEstablished the same
// cycle WindowFullUp first becomes true, and Saturated already requires
// WindowFullUp — Saturated ⟹ BaselineEstablished by construction (ADR-012's
// amendment note). It stays here so Decide never divides by zero if ever
// handed an Analysis built by hand in a test that violates the invariant.
func demand(a analyzer.Analysis) (float64, bool) {
	if a.QueueGrowthRate > queueGrowthThreshold {
		return a.ServedIOPS + a.QueueGrowthRate, true
	}
	if !a.BaselineEstablished || a.Baseline.P95 <= 0 {
		return 0, false
	}
	worst := a.ReadLatency.P95
	if a.WriteLatency.P95 > worst {
		worst = a.WriteLatency.P95
	}
	ratio := float64(worst) / float64(a.Baseline.P95)
	return a.ServedIOPS * ratio, true
}

// floorTo100 and ceilTo100 round toward the 100-multiple grid in the
// direction that keeps a bound a bound: a value already known to be <= some
// upper limit must floor (rounding up could push it back over); a value
// already known to be >= some lower limit must ceil (rounding down could
// push it back under). ceiling, MaxIOPS, MinIOPS and current are always
// already on the grid (config validation, and every value this package
// itself ever produces) — only vm.SiblingP95IOPS (a measured p95, not an
// IOPS setting) and ratio*current (ratio is a user float, not necessarily
// grid-preserving) can land off it, which is exactly why these exist.
func floorTo100(v float64) int64 { return int64(math.Floor(v/billingStepIOPS)) * billingStepIOPS }
func ceilTo100(v float64) int64  { return int64(math.Ceil(v/billingStepIOPS)) * billingStepIOPS }

// guardrails is the last line before any write (design.md §3.2/§4.6/NFR-5).
// The raw demand-based value is ceiled first — never undershoot a computed
// need. Upper bounds (ceiling, and the ratio cap on scale-up) then floor;
// lower bounds (minIOPS, and the ratio floor on scale-down) then ceil — see
// floorTo100/ceilTo100. Because every intermediate value this function
// produces is already grid-aligned, the final min/max clamp between them
// stays grid-aligned too, so there is no re-rounding step to get wrong.
// changed=false means there is no meaningful write to make (clamped back
// to current, or the remaining delta is below minChangeIOPS).
func guardrails(raw float64, current, ceiling, minIOPS int64, ratio float64, minChangeIOPS int64) (int64, bool) {
	if raw < 0 {
		raw = 0
	}
	target := ceilTo100(raw)

	upper := ceiling
	if ratio >= 1 { // scale-up: ratio also caps how high
		if cap := floorTo100(float64(current) * ratio); cap < upper {
			upper = cap
		}
	}
	upper = floorTo100(float64(upper))

	lower := minIOPS
	if ratio < 1 { // scale-down: ratio also floors how low
		if floor := ceilTo100(float64(current) * ratio); floor > lower {
			lower = floor
		}
	}
	lower = ceilTo100(float64(lower))

	if target > upper {
		target = upper
	}
	if target < lower {
		target = lower
	}

	delta := target - current
	if delta < 0 {
		delta = -delta
	}
	if delta < minChangeIOPS {
		return current, false
	}
	return target, true
}
