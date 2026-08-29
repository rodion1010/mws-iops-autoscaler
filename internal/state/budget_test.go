package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var epoch = time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)

func TestLoadBudget_missingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	if got := b.CountSince("disk1", epoch.Add(-BudgetWindow)); got != 0 {
		t.Errorf("CountSince on a fresh budget = %d, want 0", got)
	}
}

func TestBudget_recordThenCountSince(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := b.Record("disk1", epoch.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := b.Record("disk2", epoch); err != nil {
		t.Fatalf("Record disk2: %v", err)
	}

	if got := b.CountSince("disk1", epoch.Add(-time.Hour)); got != 3 {
		t.Errorf("CountSince(disk1) = %d, want 3", got)
	}
	if got := b.CountSince("disk2", epoch.Add(-time.Hour)); got != 1 {
		t.Errorf("CountSince(disk2) = %d, want 1", got)
	}
}

func TestBudget_countSinceExcludesEventsBeforeCutoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)

	b.Record("disk1", epoch.Add(-25*time.Hour)) // outside the trailing 24h
	b.Record("disk1", epoch.Add(-1*time.Hour))  // inside

	if got := b.CountSince("disk1", epoch.Add(-BudgetWindow)); got != 1 {
		t.Errorf("CountSince = %d, want 1 (only the event within the trailing 24h)", got)
	}
}

func TestLoadBudget_survivesRestart(t *testing.T) {
	// This is the whole point of ADR-013: a restart must not reset the
	// budget. Simulate one by writing with one Budget instance and
	// reloading with a fresh one over the same file.
	path := filepath.Join(t.TempDir(), "budget.jsonl")

	b1, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget (first): %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := b1.Record("disk1", epoch.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	b2, err := LoadBudget(path) // fresh instance, same file — the "restart"
	if err != nil {
		t.Fatalf("LoadBudget (after restart): %v", err)
	}
	if got := b2.CountSince("disk1", epoch.Add(-time.Hour)); got != 5 {
		t.Errorf("CountSince after reload = %d, want 5 — a restart must not reset the budget (ADR-013)", got)
	}
}

func TestLoadBudget_skipsCorruptedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b1, _ := LoadBudget(path)
	b1.Record("disk1", epoch)

	// Append a corrupted line directly.
	appendRaw(t, path, "{not valid json\n")

	b2, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget with a corrupted line present: %v", err)
	}
	if got := b2.CountSince("disk1", epoch.Add(-time.Hour)); got != 1 {
		t.Errorf("CountSince = %d, want 1 — the one good line must still be recovered", got)
	}
}

func TestBudget_compactDropsOldEventsKeepsRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)
	b.Record("disk1", epoch.Add(-30*time.Hour)) // will be dropped
	b.Record("disk1", epoch.Add(-1*time.Hour))  // kept

	if err := b.Compact(epoch.Add(-BudgetWindow)); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := len(b.events); got != 1 {
		t.Fatalf("in-memory events after Compact = %d, want 1", got)
	}

	// Reload from disk to confirm the file itself was actually rewritten,
	// not just the in-memory mirror.
	reloaded, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget after Compact: %v", err)
	}
	if got := len(reloaded.events); got != 1 {
		t.Errorf("events on disk after Compact = %d, want 1", got)
	}
}

func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s for corruption test: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatalf("write corrupted line: %v", err)
	}
}
