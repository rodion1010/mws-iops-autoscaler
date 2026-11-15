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

// Tracker combines durable budget state with in-memory cooldown and resize state.
type Tracker struct {
	mu            sync.Mutex
	disks         map[string]*diskState
	budget        *Budget
	budgetHealthy bool // sticky false once any budget persistence op fails; NewTracker sets it true
}

func NewTracker(budget *Budget) *Tracker {
	return &Tracker{disks: make(map[string]*diskState), budget: budget, budgetHealthy: true}
}

// State returns the guardrail state for a disk at now.
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
		// Fail closed through the existing exhausted-budget path when persistence is unhealthy.
		s.ChangesInLast24h = math.MaxInt
		return s
	}
	s.ChangesInLast24h = t.budget.CountSince(diskID, now.Add(-BudgetWindow))
	return s
}

// Healthy reports whether all durable budget operations have succeeded in this process.
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

// BeginResize marks a process-local write in flight until EndResize is called.
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

// ReserveChange durably consumes a budget slot before actuation.
// On failure the caller must not write and the tracker remains unhealthy.
func (t *Tracker) ReserveChange(diskID, idempotencyKey string, before, target int64, now time.Time) error {
	if err := t.budget.Reserve(diskID, idempotencyKey, before, target, now); err != nil {
		t.markUnhealthy()
		return err
	}
	return nil
}

// ConfirmChange records a verified write; persistence failure still marks the tracker unhealthy.
func (t *Tracker) ConfirmChange(idempotencyKey string) error {
	if err := t.budget.Confirm(idempotencyKey); err != nil {
		t.markUnhealthy()
		return err
	}
	return nil
}

// ReleaseChange frees a reservation that the caller has conclusively verified was not applied.
func (t *Tracker) ReleaseChange(idempotencyKey string) error {
	if err := t.budget.Release(idempotencyKey); err != nil {
		t.markUnhealthy()
		return err
	}
	return nil
}

// PendingChanges returns reservations whose outcome must be reconciled against live state.
func (t *Tracker) PendingChanges() []ChangeEvent {
	return t.budget.Pending()
}

// Compact prunes old resolved events while retaining every pending reservation.
func (t *Tracker) Compact(now time.Time) error {
	return t.budget.Compact(now.Add(-BudgetWindow))
}

// RecordChange starts the in-memory cooldown only after a write is verified.
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

// disk returns mutable per-disk state; callers must hold t.mu.
func (t *Tracker) disk(diskID string) *diskState {
	ds, ok := t.disks[diskID]
	if !ok {
		ds = &diskState{}
		t.disks[diskID] = ds
	}
	return ds
}
