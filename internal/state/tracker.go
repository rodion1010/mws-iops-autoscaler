package state

import (
	"math"
	"sync"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
)

type diskState struct {
	lastChangeUp, lastChangeDown time.Time
	resizeInProgress             bool
}

// Tracker turns Budget (durable) plus in-memory cooldown/resize state into
// the decider.GuardrailState Decide needs, and records what Decide's
// caller actually did back into both.
type Tracker struct {
	mu            sync.Mutex
	disks         map[string]*diskState
	budget        *Budget
	budgetHealthy bool // sticky false once any budget persistence op fails (issue #10) — zero-value default is fine since NewTracker sets it true explicitly
}

func NewTracker(budget *Budget) *Tracker {
	return &Tracker{disks: make(map[string]*diskState), budget: budget, budgetHealthy: true}
}

// State returns diskID's current GuardrailState as of now, for Decide.
func (t *Tracker) State(diskID string, now time.Time) decider.GuardrailState {
	t.mu.Lock()
	defer t.mu.Unlock()

	var s decider.GuardrailState
	if ds, ok := t.disks[diskID]; ok {
		s.LastChangeUp = ds.lastChangeUp
		s.LastChangeDown = ds.lastChangeDown
		s.ResizeInProgress = ds.resizeInProgress
	}
	if !t.budgetHealthy {
		// Fail closed (issue #10): once the durable budget can no longer
		// be trusted (a persistence operation failed mid-run), report it
		// as exhausted rather than whatever count happens to still be in
		// memory — reusing Decide's own existing budget_exhausted path
		// rather than adding a second, parallel "refuse to act"
		// mechanism the Decider would need to know about.
		s.ChangesInLast24h = math.MaxInt
		return s
	}
	s.ChangesInLast24h = t.budget.CountSince(diskID, now.Add(-BudgetWindow))
	return s
}

// Healthy reports whether the durable budget can currently be trusted —
// false once any Reserve/Confirm/Release call has failed this process's
// lifetime (issue #10). Agent.actuate checks this before ever calling the
// Actuator; the check inside State() above means Decide would refuse on
// its own regardless, but Healthy lets the caller avoid the write attempt
// (and the ReserveChange call that would itself just fail again) entirely.
func (t *Tracker) Healthy() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.budgetHealthy
}

func (t *Tracker) markUnhealthy() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.budgetHealthy = false
}

// BeginResize marks diskID as having a write in flight — Decide must never
// see ResizeInProgress=false while a previous write from this same process
// hasn't finished (design.md §4.3: "if resize_in_progress(disk): skip").
// Call EndResize once that write (succeeded or failed) returns; a crash
// between the two leaves ResizeInProgress stale in memory only until the
// next process restart, which starts with a clean map — this process's own
// notion of "in flight," not a claim about the platform (design.md §6.1:
// "Agent dies after scale-up: disk stays at the raised limit. No
// dead-man's switch is possible from outside the platform").
func (t *Tracker) BeginResize(diskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.disk(diskID).resizeInProgress = true
}

func (t *Tracker) EndResize(diskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.disk(diskID).resizeInProgress = false
}

// ReserveChange durably reserves diskID's budget slot for an
// about-to-be-attempted change, before any write is made (issue #10,
// reopened). The caller must not proceed to the Actuator if this returns
// an error — Tracker is now permanently unhealthy for this process, and
// every subsequent State() call reports the budget exhausted regardless.
func (t *Tracker) ReserveChange(diskID, idempotencyKey string, before, target int64, now time.Time) error {
	if err := t.budget.Reserve(diskID, idempotencyKey, before, target, now); err != nil {
		t.markUnhealthy()
		return err
	}
	return nil
}

// ConfirmChange marks a reservation as a verified, applied change. A
// failure here does not lose the count (Reserve already durably recorded
// it) but still marks the budget unhealthy — issue #10's crash-safety
// guarantee depends on every persistence operation succeeding, not just
// the first one, since an inconsistent on-disk state is exactly what a
// future restart's reconciliation has to reason correctly about.
func (t *Tracker) ConfirmChange(idempotencyKey string) error {
	if err := t.budget.Confirm(idempotencyKey); err != nil {
		t.markUnhealthy()
		return err
	}
	return nil
}

// ReleaseChange marks a reservation as conclusively not applied, freeing
// its budget slot. See Budget.Release for the verification obligation on
// the caller before calling this.
func (t *Tracker) ReleaseChange(idempotencyKey string) error {
	if err := t.budget.Release(idempotencyKey); err != nil {
		t.markUnhealthy()
		return err
	}
	return nil
}

// PendingChanges exposes every reservation this process (or a previous
// one, before a crash) never resolved — the set a caller must reconcile
// against live disk state before the budget can be fully trusted again
// (issue #10).
func (t *Tracker) PendingChanges() []ChangeEvent {
	return t.budget.Pending()
}

// Compact prunes the durable budget log, keeping every still-pending
// reservation regardless of age plus every resolved event within
// BudgetWindow of now. See Budget.Compact.
func (t *Tracker) Compact(now time.Time) error {
	return t.budget.Compact(now.Add(-BudgetWindow))
}

// RecordChange records diskID's cooldown clock for the given direction —
// in-memory only (ADR-013), and called only once a write has been
// verified successful. Issue #10 split this from budget bookkeeping,
// which now happens via ReserveChange/ConfirmChange/ReleaseChange around
// the write itself rather than only after it succeeds.
func (t *Tracker) RecordChange(diskID string, up bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ds := t.disk(diskID)
	if up {
		ds.lastChangeUp = now
	} else {
		ds.lastChangeDown = now
	}
}

// disk returns diskID's mutable state, creating it on first use. Callers
// must already hold t.mu.
func (t *Tracker) disk(diskID string) *diskState {
	ds, ok := t.disks[diskID]
	if !ok {
		ds = &diskState{}
		t.disks[diskID] = ds
	}
	return ds
}
