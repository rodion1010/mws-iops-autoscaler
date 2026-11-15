package decider

import (
	"math"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
)

// platformMaxIOPS is the disk-level platform ceiling included in every clamp.
const platformMaxIOPS = 10000

// billingStepIOPS mirrors config.BillingStepIOPS without coupling the packages.
const billingStepIOPS = 100

// queueGrowthThreshold separates a growing open-workload queue from measurement noise.
// It is calibration-sensitive.
const queueGrowthThreshold = 0.01

// PolicyDecider is stateless across decisions.
type PolicyDecider struct{}

func NewPolicyDecider() *PolicyDecider {
	return &PolicyDecider{}
}

func (pd *PolicyDecider) Decide(diskID string, current int64, a analyzer.Analysis, vm VMState, p EffectivePolicy, state GuardrailState, now time.Time) Decision {
	base := Decision{
		DiskID:  diskID,
		Current: current,
		Target:  current,
		Inputs:  a,
		// VM headroom is separate from disk policy/platform ceilings.
		// BudgetRemaining describes standing state even when no action is taken.
		VMHeadroomIOPS:  vm.IOPSPool - int64(vm.SiblingP95IOPS),
		BudgetRemaining: p.MaxChangesPer24h - state.ChangesInLast24h,
	}

	if state.ResizeInProgress {
		return withReason(base, ActionNoAction, "resize_in_progress")
	}

	ceiling := headroomCeiling(vm, p)

	switch {
	case a.Saturated:
		// An unmeasurable sibling counts as zero in SiblingP95IOPS, so the
		// ceiling may overstate headroom. ActionNoAction, not
		// ActionVMCapReached: vm_cap_reached means "add vCPU", while a
		// missing sibling sample needs that disk's metrics path fixed.
		if vm.SiblingDemandUnknown {
			return withReason(base, ActionNoAction, "sibling_demand_unmeasurable")
		}
		// Raw VM headroom, not ceiling: a disk at its own max_iops has
		// ceiling <= current with pool to spare, and must fall through to
		// target_not_above_current rather than raise the add-vCPU signal.
		if base.VMHeadroomIOPS <= current {
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
		base.Demand = a.EffectiveDemandP95Down
		// A quiet reading can be VM-pool contention, not real idleness.
		// Raw VM headroom, not ceiling: a disk at its own max_iops has
		// ceiling <= current without any VM contention.
		if vm.SiblingDemandUnknown {
			return withReason(base, ActionNoAction, "sibling_demand_unmeasurable")
		}
		if base.VMHeadroomIOPS <= current {
			return withReason(base, ActionNoAction, "vm_pool_contended")
		}
		if now.Sub(state.LastChangeDown) < p.CooldownScaleDown {
			return withReason(base, ActionNoAction, "cooldown_scale_down")
		}
		raw := a.EffectiveDemandP95Down / p.ScaleDownUtilization
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
		// Ordering distinguishes cold start, stale data, incomplete windows, and a real no-signal state.
		// HasData must precede Fresh because both are false during cold start.
		switch {
		case !a.HasData:
			return withReason(base, ActionNoAction, "no_data")
		case !a.Fresh:
			return withReason(base, ActionNoAction, "metrics_stale")
		case !a.WindowFullUp && !a.WindowFullDown:
			return withReason(base, ActionNoAction, "window_not_full")
		default:
			return withReason(base, ActionNoAction, "no_signal")
		}
	}
}

func withReason(d Decision, action Action, reason string) Decision {
	d.Action = action
	d.Reason = reason
	return d
}

// headroomCeiling combines platform, policy, and VM aggregate limits.
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

// demand uses queue growth for open workloads and latency ratio for closed workloads.
// The missing-baseline branch defensively avoids division by zero for malformed Analysis values.
func demand(a analyzer.Analysis) (float64, bool) {
	// Queue growth can be added to served IOPS only when both are ops-denominated.
	// Invalid units fall back to the closed-workload regime.
	if a.QueueGrowthRateValid && a.QueueGrowthRate > queueGrowthThreshold {
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

// floorTo100 and ceilTo100 preserve upper and lower bounds while aligning to billing steps.
// Measured and ratio-derived inputs may be off-grid even though configured limits are not.
func floorTo100(v float64) int64 { return int64(math.Floor(v/billingStepIOPS)) * billingStepIOPS }
func ceilTo100(v float64) int64  { return int64(math.Ceil(v/billingStepIOPS)) * billingStepIOPS }

// guardrails aligns demand upward, upper bounds downward, and lower bounds upward.
// changed is false when clamping returns to current or the remaining delta is below the minimum.
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
