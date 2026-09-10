// Package decider turns an Analyzer's evidence (internal/analyzer.Analysis)
// into a clamped, rate-limited Decision — demand estimation and guardrails
// (design.md §4.5/§3.2, M4). It is pure: cooldown/budget state is read from
// an explicit GuardrailState the caller resolves, never owned or persisted
// here (design.md §3.3's "chain of pure functions" principle, and docs/design.md §9.4
// — the 24h budget's durable record lives outside this package).
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

// VMState is the sibling-disk headroom picture (design.md §4.6),
// pre-aggregated by the caller — IOPSPool and every other managed disk's
// own Analysis are already available from the same reconcile cycle, so
// recomputing them inside Decide would just redo work the caller already
// did.
type VMState struct {
	IOPSPool       int64   // 3000 × vCPU (Discover, M1) — not enforced by the platform
	SiblingP95IOPS float64 // aggregate p95 IOPS of every other disk sharing the VM's pool

	// SiblingDemandUnknown is true when at least one other disk sharing the
	// VM's IOPS pool could not be reliably measured this cycle (no sample
	// yet, or the sample is stale) — that disk's real consumption is folded
	// into SiblingP95IOPS as zero, understating it rather than reporting it
	// as unknown. Decide must not approve a scale-up against headroom that
	// might already be spent (issue #56) — blocked as ActionNoAction, not
	// ActionVMCapReached: the gap is in measurement, not the VM's pool, and
	// must not alias onto the vCPU-upgrade signal that action drives.
	SiblingDemandUnknown bool
}

// EffectivePolicy is the narrow slice of the resolved policy Decide needs —
// a plain struct decoupled from internal/config, the same way
// analyzer.EvaluationPolicy is (design.md §3.3).
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

// GuardrailState is cooldown/budget state as of now, resolved by the caller
// from wherever it is tracked (docs/design.md §9.4: the 24h counter is persisted,
// cooldown timestamps are not). ResizeInProgress covers both directions:
// one in-flight write blocks either.
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

	// VMHeadroomIOPS and BudgetRemaining are carried forward from vm/state
	// (§4.6, docs/design.md §9.4) so a journal/metrics consumer (M5) doesn't need
	// those inputs threaded through separately just to report on them —
	// Decide already computed/received them.
	VMHeadroomIOPS  int64
	BudgetRemaining int
}

// Decider is implemented by PolicyDecider (decider.go); an interface so
// callers can substitute a fake in tests the same way the rest of this
// codebase does (internal/compute's fakeClient, internal/analyzer's
// Analyzer).
type Decider interface {
	// diskID/current mirror analyzer.Analyzer.Analyze's explicit-diskID
	// rationale — Decision.DiskID/Current must be set even when a is a
	// zero-value Analysis. now is explicit so cooldown math is testable
	// without a real clock.
	Decide(diskID string, current int64, a analyzer.Analysis, vm VMState, p EffectivePolicy, state GuardrailState, now time.Time) Decision
}
