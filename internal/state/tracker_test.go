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
	if err := tr.RecordChange("disk1", true, epoch); err != nil {
		t.Fatalf("RecordChange(up): %v", err)
	}

	s := tr.State("disk1", epoch)
	if s.LastChangeUp != epoch {
		t.Errorf("LastChangeUp = %v, want %v", s.LastChangeUp, epoch)
	}
	if !s.LastChangeDown.IsZero() {
		t.Errorf("LastChangeDown = %v, want zero — a scale-up must not touch the scale-down cooldown", s.LastChangeDown)
	}
}

func TestTracker_recordChangeUpdatesBudget(t *testing.T) {
	tr := newTestTracker(t)
	for i := 0; i < 3; i++ {
		if err := tr.RecordChange("disk1", true, epoch.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("RecordChange: %v", err)
		}
	}
	if got := tr.State("disk1", epoch.Add(time.Hour)).ChangesInLast24h; got != 3 {
		t.Errorf("ChangesInLast24h = %d, want 3", got)
	}
}

func TestTracker_budgetSurvivesRestartCooldownDoesNot(t *testing.T) {
	// The exact property ADR-013 exists for: reload the budget from the
	// same file (simulating a restart) into a *new* Tracker, and confirm
	// the count carries over while cooldown state — correctly — starts
	// fresh, since a new Tracker has no in-memory history at all.
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b1, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget (first): %v", err)
	}
	tr1 := NewTracker(b1)
	for i := 0; i < 4; i++ {
		if err := tr1.RecordChange("disk1", true, epoch.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("RecordChange: %v", err)
		}
	}

	b2, err := LoadBudget(path) // "restart"
	if err != nil {
		t.Fatalf("LoadBudget (after restart): %v", err)
	}
	tr2 := NewTracker(b2)

	s := tr2.State("disk1", epoch.Add(time.Hour))
	if s.ChangesInLast24h != 4 {
		t.Errorf("ChangesInLast24h after restart = %d, want 4 — the budget must survive (ADR-013)", s.ChangesInLast24h)
	}
	if !s.LastChangeUp.IsZero() {
		t.Errorf("LastChangeUp after restart = %v, want zero — cooldowns are deliberately not persisted", s.LastChangeUp)
	}
}
