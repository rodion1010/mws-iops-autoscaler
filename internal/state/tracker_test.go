package state

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestTracker(t *testing.T) *Tracker {
	t.Helper()
	b, err := LoadBudget(filepath.Join(t.TempDir(), "budget.jsonl"))
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	return NewTracker(b)
}

func TestTracker_stateIsZeroForUnknownDisk(t *testing.T) {
	tr := newTestTracker(t)
	s := tr.State("disk1", epoch)
	if s.ResizeInProgress || !s.LastChangeUp.IsZero() || !s.LastChangeDown.IsZero() || s.ChangesInLast24h != 0 {
		t.Errorf("State for an unknown disk = %+v, want the zero value", s)
	}
}

func TestTracker_beginEndResize(t *testing.T) {
	tr := newTestTracker(t)
	tr.BeginResize("disk1")
	if !tr.State("disk1", epoch).ResizeInProgress {
		t.Error("ResizeInProgress = false after BeginResize")
	}
	tr.EndResize("disk1")
	if tr.State("disk1", epoch).ResizeInProgress {
		t.Error("ResizeInProgress = true after EndResize")
	}
}

func TestTracker_recordChangeSetsCooldownDirectionSpecifically(t *testing.T) {
	tr := newTestTracker(t)
	tr.RecordChange("disk1", true, epoch)

	s := tr.State("disk1", epoch)
	if s.LastChangeUp != epoch {
		t.Errorf("LastChangeUp = %v, want %v", s.LastChangeUp, epoch)
	}
	if !s.LastChangeDown.IsZero() {
		t.Errorf("LastChangeDown = %v, want zero — a scale-up must not touch the scale-down cooldown", s.LastChangeDown)
	}
}

func TestTracker_reserveConfirmUpdatesBudget(t *testing.T) {
	tr := newTestTracker(t)
	for i := 0; i < 3; i++ {
		key := keyN(i)
		ts := epoch.Add(time.Duration(i) * time.Minute)
		if err := tr.ReserveChange("disk1", key, 1000, 2000, ts); err != nil {
			t.Fatalf("ReserveChange: %v", err)
		}
		if err := tr.ConfirmChange(key); err != nil {
			t.Fatalf("ConfirmChange: %v", err)
		}
	}
	if got := tr.State("disk1", epoch.Add(time.Hour)).ChangesInLast24h; got != 3 {
		t.Errorf("ChangesInLast24h = %d, want 3", got)
	}
}

func TestTracker_reserveWithoutResolutionStillCounts(t *testing.T) {
	// A pending reservation still consumes budget.
	tr := newTestTracker(t)
	if err := tr.ReserveChange("disk1", "key-1", 1000, 2000, epoch); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}
	if got := tr.State("disk1", epoch.Add(time.Minute)).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h with an unresolved reservation = %d, want 1", got)
	}
}

func TestTracker_releaseChangeFreesTheSlot(t *testing.T) {
	tr := newTestTracker(t)
	if err := tr.ReserveChange("disk1", "key-1", 1000, 2000, epoch); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}
	if err := tr.ReleaseChange("key-1"); err != nil {
		t.Fatalf("ReleaseChange: %v", err)
	}
	if got := tr.State("disk1", epoch.Add(time.Minute)).ChangesInLast24h; got != 0 {
		t.Errorf("ChangesInLast24h after Release = %d, want 0", got)
	}
}

func TestTracker_budgetSurvivesRestartCooldownDoesNot(t *testing.T) {
	// Durable budget survives restart while in-memory cooldown resets.
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b1, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget (first): %v", err)
	}
	tr1 := NewTracker(b1)
	for i := 0; i < 4; i++ {
		key := keyN(i)
		ts := epoch.Add(time.Duration(i) * time.Minute)
		if err := tr1.ReserveChange("disk1", key, 1000, 2000, ts); err != nil {
			t.Fatalf("ReserveChange: %v", err)
		}
		if err := tr1.ConfirmChange(key); err != nil {
			t.Fatalf("ConfirmChange: %v", err)
		}
		tr1.RecordChange("disk1", true, ts)
	}

	b2, err := LoadBudget(path) // "restart"
	if err != nil {
		t.Fatalf("LoadBudget (after restart): %v", err)
	}
	tr2 := NewTracker(b2)

	s := tr2.State("disk1", epoch.Add(time.Hour))
	if s.ChangesInLast24h != 4 {
		t.Errorf("ChangesInLast24h after restart = %d, want 4 — the budget must survive (docs/design.md §9.4)", s.ChangesInLast24h)
	}
	if !s.LastChangeUp.IsZero() {
		t.Errorf("LastChangeUp after restart = %v, want zero — cooldowns are deliberately not persisted", s.LastChangeUp)
	}
}

func TestTracker_healthyByDefault(t *testing.T) {
	tr := newTestTracker(t)
	if !tr.Healthy() {
		t.Error("Healthy() = false on a freshly constructed Tracker, want true")
	}
}

func failingBudgetPath(t *testing.T) string {
	t.Helper()
	// A missing parent directory fails deterministically for every user.
	return filepath.Join(t.TempDir(), "does-not-exist", "budget.jsonl")
}

func TestTracker_reserveFailureMarksUnhealthyAndFailsClosed(t *testing.T) {
	b := &Budget{path: failingBudgetPath(t)}
	tr := NewTracker(b)

	if err := tr.ReserveChange("disk1", "key-1", 1000, 2000, epoch); err == nil {
		t.Fatal("ReserveChange succeeded against an unwritable path")
	}
	if tr.Healthy() {
		t.Error("Healthy() = true after a Reserve failure, want false")
	}

	s := tr.State("disk1", epoch.Add(time.Minute))
	if s.ChangesInLast24h < 1_000_000 {
		t.Errorf("ChangesInLast24h once unhealthy = %d, want a sentinel large enough that budget_exhausted always fires (fail closed, issue #10)", s.ChangesInLast24h)
	}
}

func TestTracker_confirmFailureMarksUnhealthy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	tr := NewTracker(b)
	if err := tr.ReserveChange("disk1", "key-1", 1000, 2000, epoch); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	// Move the live Budget to a missing directory to force the next append to fail.
	b.path = failingBudgetPath(t)

	if err := tr.ConfirmChange("key-1"); err == nil {
		t.Fatal("ConfirmChange succeeded despite an unwritable budget path")
	}
	if tr.Healthy() {
		t.Error("Healthy() = true after a Confirm failure, want false")
	}
}

func TestTracker_pendingChangesExposesUnresolvedReservations(t *testing.T) {
	tr := newTestTracker(t)
	if err := tr.ReserveChange("disk1", "key-1", 1000, 2000, epoch); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}
	if err := tr.ReserveChange("disk2", "key-2", 3000, 4000, epoch); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}
	if err := tr.ConfirmChange("key-2"); err != nil {
		t.Fatalf("ConfirmChange: %v", err)
	}

	pending := tr.PendingChanges()
	if len(pending) != 1 || pending[0].IdempotencyKey != "key-1" {
		t.Errorf("PendingChanges() = %+v, want only key-1 (key-2 was confirmed)", pending)
	}
}

func TestTracker_compactDelegatesToBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	tr := NewTracker(b)
	if err := tr.ReserveChange("disk1", "key-1", 1000, 2000, epoch.Add(-30*time.Hour)); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}
	if err := tr.ConfirmChange("key-1"); err != nil {
		t.Fatalf("ConfirmChange: %v", err)
	}

	if err := tr.Compact(epoch); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := tr.State("disk1", epoch).ChangesInLast24h; got != 0 {
		t.Errorf("ChangesInLast24h after Compact = %d, want 0 — the 30h-old confirmed event should have been pruned", got)
	}
}
