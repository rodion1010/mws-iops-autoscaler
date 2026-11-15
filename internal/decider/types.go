// Package decider turns analyzer evidence into a clamped, rate-limited
// Decision. It is pure: cooldown and budget state come from a GuardrailState
// the caller resolves; nothing is owned or persisted here.
package decider

import (
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
)

// Action is what Decide concluded.
type Action string

const (
	ActionScaleUp      Action = "scale_up"
	ActionScaleDown    Action = "scale_down"
	ActionNoAction     Action = "no_action"
	ActionVMCapReached Action = "vm_cap_reached"
)

// VMState is the VM IOPS-pool picture shared by sibling disks, pre-aggregated
// by the caller from the same reconcile cycle.
type VMState struct {
	IOPSPool       int64   // 3000 × vCPU; not enforced by the platform
	SiblingP95IOPS float64 // aggregate p95 IOPS of every other disk sharing the VM's pool

	// SiblingDemandUnknown is true when at least one other disk sharing the
	// pool has no fresh sample this cycle; its consumption is counted as zero
	// in SiblingP95IOPS. Decide then blocks resizes with ActionNoAction, not
	// ActionVMCapReached: the gap is in measurement, not in the VM's pool.
	SiblingDemandUnknown bool
}

// EffectivePolicy is the slice of the resolved policy Decide needs, kept
// independent of internal/config.
type EffectivePolicy struct {
	MinIOPS int64
	MaxIOPS int64

	ScaleUpUtilization   float64
	ScaleDownUtilization float64

	CooldownScaleUp   time.Duration
	CooldownScaleDown time.Duration

	MinChangeIOPS     int64
	MaxScaleUpRatio   float64
	MaxScaleDownRatio float64

	MaxChangesPer24h int
}

// GuardrailState is cooldown and budget state as of now, resolved by the
// caller. ResizeInProgress blocks both directions: one in-flight write blocks
// either.
type GuardrailState struct {
	ResizeInProgress bool
	LastChangeUp     time.Time // zero value = never
	LastChangeDown   time.Time
	ChangesInLast24h int
}

// Decision is Decide's output.
type Decision struct {
	DiskID  string
	Action  Action
	Reason  string // machine-parseable
	Current int64
	Target  int64 // == Current when Action is ActionNoAction or ActionVMCapReached
	Demand  float64
	Inputs  analyzer.Analysis // full evidence, for the journal

	// VMHeadroomIOPS and BudgetRemaining are carried forward so journal and
	// metrics consumers can report them without the original inputs.
	VMHeadroomIOPS  int64
	BudgetRemaining int
}

// Decider is implemented by PolicyDecider; an interface so callers can
// substitute a fake in tests.
type Decider interface {
	// Decide sets Decision.DiskID and Current even when a is a zero-value
	// Analysis. now is explicit so cooldown math is testable without a real
	// clock.
	Decide(diskID string, current int64, a analyzer.Analysis, vm VMState, p EffectivePolicy, state GuardrailState, now time.Time) Decision
}
