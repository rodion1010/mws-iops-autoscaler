package state

import (
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
	mu     sync.Mutex
	disks  map[string]*diskState
	budget *Budget
}

func NewTracker(budget *Budget) *Tracker {
	return &Tracker{disks: make(map[string]*diskState), budget: budget}
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
	s.ChangesInLast24h = t.budget.CountSince(diskID, now.Add(-BudgetWindow))
	return s
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

// RecordChange records that diskID actually changed at now, in the
// direction up indicates — both the in-memory cooldown clock
// (direction-specific: ADR-010/ADR-012 already established a two-direction
// concept never collapses to one flag, and cooldown is the same shape
// again) and the durable budget. Returns the persistence error, if any, so
// the caller can decide how to react; the in-memory cooldown has already
// taken effect regardless; logging and continuing is reasonable — a
// missed persist only risks the budget undercounting by one change, not
// overcounting into a false block.
func (t *Tracker) RecordChange(diskID string, up bool, now time.Time) error {
	t.mu.Lock()
	ds := t.disk(diskID)
	if up {
		ds.lastChangeUp = now
	} else {
		ds.lastChangeDown = now
	}
	t.mu.Unlock()

	return t.budget.Record(diskID, now)
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
