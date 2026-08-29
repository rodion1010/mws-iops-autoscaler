package state

import (
	"os"
	"path/filepath"
	"strings"
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

// reserveConfirm is the common "attempt a change and it landed" sequence
// used throughout this file — Reserve then Confirm, matching what
// Agent.actuate does around a successful write.
func reserveConfirm(t *testing.T, b *Budget, diskID, key string, before, target int64, ts time.Time) {
	t.Helper()
	if err := b.Reserve(diskID, key, before, target, ts); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := b.Confirm(key); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
}

func TestBudget_reserveConfirmThenCountSince(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}

	for i := 0; i < 3; i++ {
		reserveConfirm(t, b, "disk1", keyN(i), 1000, 2000, epoch.Add(time.Duration(i)*time.Minute))
	}
	reserveConfirm(t, b, "disk2", "disk2-key", 1000, 2000, epoch)

	if got := b.CountSince("disk1", epoch.Add(-time.Hour)); got != 3 {
		t.Errorf("CountSince(disk1) = %d, want 3", got)
	}
	if got := b.CountSince("disk2", epoch.Add(-time.Hour)); got != 1 {
		t.Errorf("CountSince(disk2) = %d, want 1", got)
	}
}

func TestBudget_pendingCountsTheSameAsConfirmed(t *testing.T) {
	// issue #10: undercounting is the dangerous direction, so a
	// reservation that was never resolved (this run crashed, or simply
	// hasn't gotten to Confirm yet) must still consume the budget.
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)

	if err := b.Reserve("disk1", "key-1", 1000, 2000, epoch); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got := b.CountSince("disk1", epoch.Add(-time.Hour)); got != 1 {
		t.Errorf("CountSince with only a pending reservation = %d, want 1", got)
	}
}

func TestBudget_releasedDoesNotCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)

	if err := b.Reserve("disk1", "key-1", 1000, 2000, epoch); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := b.Release("key-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := b.CountSince("disk1", epoch.Add(-time.Hour)); got != 0 {
		t.Errorf("CountSince after Release = %d, want 0 — a conclusively-not-applied write must not consume budget", got)
	}
}

func TestBudget_countSinceExcludesEventsBeforeCutoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)

	reserveConfirm(t, b, "disk1", "old", 1000, 2000, epoch.Add(-25*time.Hour)) // outside the trailing 24h
	reserveConfirm(t, b, "disk1", "new", 1000, 2000, epoch.Add(-1*time.Hour))  // inside

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
		reserveConfirm(t, b1, "disk1", keyN(i), 1000, 2000, epoch.Add(time.Duration(i)*time.Minute))
	}

	b2, err := LoadBudget(path) // fresh instance, same file — the "restart"
	if err != nil {
		t.Fatalf("LoadBudget (after restart): %v", err)
	}
	if got := b2.CountSince("disk1", epoch.Add(-time.Hour)); got != 5 {
		t.Errorf("CountSince after reload = %d, want 5 — a restart must not reset the budget (ADR-013)", got)
	}
}

func TestLoadBudget_survivesRestartWithPendingReservation(t *testing.T) {
	// The literal issue #10 scenario: a crash between Reserve and
	// Confirm/Release must not lose the reservation on reload.
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b1, _ := LoadBudget(path)
	if err := b1.Reserve("disk1", "key-1", 1000, 2000, epoch); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// No Confirm/Release — simulates a crash right here.

	b2, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget after crash: %v", err)
	}
	if got := b2.CountSince("disk1", epoch.Add(-time.Hour)); got != 1 {
		t.Errorf("CountSince after reload = %d, want 1 — a crash mid-reservation must not lose the count", got)
	}
	pending := b2.Pending()
	if len(pending) != 1 || pending[0].IdempotencyKey != "key-1" {
		t.Errorf("Pending() = %+v, want one entry for key-1", pending)
	}
}

func TestLoadBudget_rejectsCorruptionInTheMiddle(t *testing.T) {
	// issue #10, reopened: corruption anywhere but the last line must
	// fail closed, not be silently skipped — the previous behavior could
	// only ever make the budget look more permissive than reality.
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b1, _ := LoadBudget(path)
	reserveConfirm(t, b1, "disk1", "key-1", 1000, 2000, epoch)
	reserveConfirm(t, b1, "disk1", "key-2", 1000, 2000, epoch.Add(time.Minute))

	// Splice a corrupted line into the middle of the file.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read budget file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("test setup: expected at least 3 lines, got %d", len(lines))
	}
	lines = append(lines[:1], append([]string{"{not valid json"}, lines[1:]...)...)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite budget file with corruption: %v", err)
	}

	_, err = LoadBudget(path)
	if err == nil {
		t.Fatal("LoadBudget succeeded despite corruption in the middle of the file — must fail closed")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error = %v, want it to name the corruption", err)
	}
}

func TestLoadBudget_recoversFromTornTailWrite(t *testing.T) {
	// issue #10, reopened: a crash mid-append can only ever leave the
	// *last* line torn (append() fsyncs before returning, so every prior
	// line is already durable) — this is recoverable, not corruption.
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b1, _ := LoadBudget(path)
	reserveConfirm(t, b1, "disk1", "key-1", 1000, 2000, epoch)

	appendRaw(t, path, `{"disk_id":"disk1","ts":"2026-08-29T00:0`) // torn: no closing, no newline

	b2, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget with a torn tail: %v, want a clean recovery", err)
	}
	if got := b2.CountSince("disk1", epoch.Add(-time.Hour)); got != 1 {
		t.Errorf("CountSince = %d, want 1 — the one complete, durable record must survive a torn tail", got)
	}

	// The recovery must itself be durable — reloading again must not see
	// the torn bytes resurface.
	b3, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget (second reload): %v", err)
	}
	if got := b3.CountSince("disk1", epoch.Add(-time.Hour)); got != 1 {
		t.Errorf("CountSince after second reload = %d, want 1", got)
	}
}

func TestBudget_compactKeepsPendingRegardlessOfAge(t *testing.T) {
	// issue #10, reopened: a still-unresolved reservation must never be
	// compacted away, no matter how old — discarding it early would
	// silently undo the undercounting guarantee Reserve exists to provide.
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)
	if err := b.Reserve("disk1", "old-pending", 1000, 2000, epoch.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := b.Compact(epoch.Add(-BudgetWindow)); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if got := b.CountSince("disk1", epoch.Add(-72*time.Hour)); got != 1 {
		t.Errorf("CountSince after Compact = %d, want 1 — a pending reservation must survive compaction regardless of age", got)
	}
	pending := b.Pending()
	if len(pending) != 1 || pending[0].IdempotencyKey != "old-pending" {
		t.Errorf("Pending() after Compact = %+v, want the old-pending reservation still present", pending)
	}
}

func TestBudget_compactDropsOldResolvedEventsKeepsRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)
	reserveConfirm(t, b, "disk1", "old", 1000, 2000, epoch.Add(-30*time.Hour)) // will be dropped
	reserveConfirm(t, b, "disk1", "new", 1000, 2000, epoch.Add(-1*time.Hour))  // kept

	if err := b.Compact(epoch.Add(-BudgetWindow)); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := b.CountSince("disk1", epoch.Add(-BudgetWindow)); got != 1 {
		t.Fatalf("CountSince after Compact = %d, want 1", got)
	}

	// Reload from disk to confirm the file itself was actually rewritten,
	// not just the in-memory mirror.
	reloaded, err := LoadBudget(path)
	if err != nil {
		t.Fatalf("LoadBudget after Compact: %v", err)
	}
	if got := reloaded.CountSince("disk1", epoch.Add(-BudgetWindow)); got != 1 {
		t.Errorf("CountSince after reload = %d, want 1", got)
	}
}

func TestBudget_reserveEmptyIdempotencyKeyRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)
	if err := b.Reserve("disk1", "", 1000, 2000, epoch); err == nil {
		t.Fatal("Reserve succeeded with an empty idempotency key")
	}
}

func TestBudget_confirmReleaseUnknownKeyFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, _ := LoadBudget(path)
	if err := b.Confirm("no-such-key"); err == nil {
		t.Error("Confirm succeeded for a key with no reservation")
	}
	if err := b.Release("no-such-key"); err == nil {
		t.Error("Release succeeded for a key with no reservation")
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

func keyN(i int) string {
	return "key-" + string(rune('a'+i))
}
