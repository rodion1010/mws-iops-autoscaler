// Package state persists safety state that must survive process restarts.
// Cooldowns remain in memory because cold-start windows are at least as long; the rolling
// change budget is durable so restarts cannot reset its hard cap.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// BudgetWindow is the rolling period used by max_changes_per_24h.
const BudgetWindow = 24 * time.Hour

// Status is a reservation's intent/result state.
// Pending and confirmed entries count against budget; only a conclusively verified non-write
// may be released.
type Status string

const (
	// StatusPending is durable intent recorded before a write and counts against budget.
	StatusPending Status = "pending"
	// StatusConfirmed is a verified write and continues to count against budget.
	StatusConfirmed Status = "confirmed"
	// StatusReleased is a verified non-write and does not count against budget.
	StatusReleased Status = "released"
)

// ChangeEvent is one self-contained record in the append-only budget log.
// Records sharing an idempotency key form a reservation and its later state transitions.
type ChangeEvent struct {
	DiskID         string    `json:"disk_id"`
	TS             time.Time `json:"ts"` // reservation time — fixed across all of a key's transitions; budget counts from here
	IdempotencyKey string    `json:"idempotency_key"`
	Before         int64     `json:"before"` // Decision.Current at reservation time — the value a released write must still match
	Target         int64     `json:"target"` // Decision.Target — the value a confirmed write must match
	Status         Status    `json:"status"`
}

// Budget owns durable file operations for the rolling change budget.
// Its single-goroutine caller provides concurrency control.
type Budget struct {
	path   string
	events []ChangeEvent // in-memory mirror, append order; the file is the durable copy
}

// LoadBudget reconstructs the change history, creating the file when absent.
// A corrupt final line is treated as a crash-torn append and removed durably; corruption
// anywhere else fails closed because the budget can no longer be trusted.
func LoadBudget(path string) (*Budget, error) {
	b := &Budget{path: path}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if cerr := createEmpty(path); cerr != nil {
			return nil, cerr
		}
		return b, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read budget file %s: %w", path, err)
	}

	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1] // trailing newline, not a line
	}

	tornTail := false
	for i, raw := range lines {
		if len(raw) == 0 {
			continue
		}
		var ev ChangeEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			if i == len(lines)-1 {
				tornTail = true
				break
			}
			return nil, fmt.Errorf(
				"state: corrupt budget file %s at line %d: %w — refusing to start with an unreliable change budget (fail closed)",
				path, i+1, err)
		}
		b.events = append(b.events, ev)
	}

	if tornTail {
		// Rewrite valid events so the next append cannot follow torn bytes.
		if err := b.writeAll(b.events); err != nil {
			return nil, fmt.Errorf("state: recover torn budget file %s: %w", path, err)
		}
	}

	return b, nil
}

func createEmpty(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("state: create budget file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close new budget file %s: %w", path, err)
	}
	return syncDir(path)
}

// latestByKey returns the last, authoritative transition for each reservation.
func (b *Budget) latestByKey() map[string]ChangeEvent {
	latest := make(map[string]ChangeEvent, len(b.events))
	for _, ev := range b.events {
		latest[ev.IdempotencyKey] = ev
	}
	return latest
}

// CountSince counts non-released reservations for a disk at or after cutoff.
// Pending entries remain charged until reconciliation proves no write occurred.
func (b *Budget) CountSince(diskID string, cutoff time.Time) int {
	n := 0
	for _, ev := range b.latestByKey() {
		if ev.DiskID != diskID || ev.TS.Before(cutoff) || ev.Status == StatusReleased {
			continue
		}
		n++
	}
	return n
}

// Pending returns reservations whose outcome still requires live-state reconciliation.
func (b *Budget) Pending() []ChangeEvent {
	var out []ChangeEvent
	for _, ev := range b.latestByKey() {
		if ev.Status == StatusPending {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

// Reserve durably consumes a budget slot before attempting the write.
// Recording intent first prevents a crash after a successful platform write from undercounting
// the hard cap; verified failures are released explicitly.
func (b *Budget) Reserve(diskID, idempotencyKey string, before, target int64, ts time.Time) error {
	if idempotencyKey == "" {
		return fmt.Errorf("state: reserve budget slot for disk %q: empty idempotency key", diskID)
	}
	return b.append(ChangeEvent{
		DiskID: diskID, TS: ts, IdempotencyKey: idempotencyKey,
		Before: before, Target: target, Status: StatusPending,
	})
}

// Confirm records that a reserved write was verified as applied.
// The reservation already counts, so confirmation does not change the total.
func (b *Budget) Confirm(idempotencyKey string) error {
	return b.transition(idempotencyKey, StatusConfirmed)
}

// Release records that a reservation was conclusively not applied.
// Callers must verify the live value still equals Before; a transport error alone is ambiguous.
func (b *Budget) Release(idempotencyKey string) error {
	return b.transition(idempotencyKey, StatusReleased)
}

func (b *Budget) transition(idempotencyKey string, status Status) error {
	var orig ChangeEvent
	found := false
	for _, ev := range b.events {
		if ev.IdempotencyKey == idempotencyKey {
			orig = ev
			found = true
		}
	}
	if !found {
		return fmt.Errorf("state: transition budget event %q to %q: no reservation found", idempotencyKey, status)
	}
	orig.Status = status
	return b.append(orig)
}

// append fsyncs each transition before returning.
func (b *Budget) append(ev ChangeEvent) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("state: encode change event: %w", err)
	}

	f, err := os.OpenFile(b.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("state: open budget file %s for append: %w", b.path, err)
	}
	defer f.Close()

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("state: append to budget file %s: %w", b.path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("state: sync budget file %s: %w", b.path, err)
	}

	b.events = append(b.events, ev)
	return nil
}

// Compact retains all pending reservations and recent resolved events.
// It atomically rewrites and renames the file, then fsyncs the containing directory so
// crashes cannot discard the durable budget.
func (b *Budget) Compact(cutoff time.Time) error {
	kept := make([]ChangeEvent, 0, len(b.events))
	for _, ev := range b.latestByKey() {
		if ev.Status == StatusPending || !ev.TS.Before(cutoff) {
			kept = append(kept, ev)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].TS.Before(kept[j].TS) })

	if err := b.writeAll(kept); err != nil {
		return err
	}
	b.events = kept
	return nil
}

// writeAll atomically replaces the log for compaction and torn-tail recovery.
func (b *Budget) writeAll(events []ChangeEvent) error {
	tmp := b.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("state: create compacted budget file: %w", err)
	}
	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			f.Close()
			return fmt.Errorf("state: write compacted budget file: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("state: sync compacted budget file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close compacted budget file: %w", err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return fmt.Errorf("state: replace budget file with compacted copy: %w", err)
	}
	return syncDir(b.path)
}

// syncDir makes a rename or file creation durable on POSIX filesystems.
func syncDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("state: open directory for fsync %s: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("state: sync directory %s: %w", filepath.Dir(path), err)
	}
	return nil
}
