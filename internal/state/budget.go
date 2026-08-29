// Package state is the durable+in-memory state Guardrails needs across
// reconcile cycles and process restarts (ADR-013): cooldown timestamps stay
// in memory — safe, because internal/config's validation enforces
// cooldown <= its paired window, so the cold-start wait after a restart is
// never shorter than the cooldown a restart forgot — but the 24h change
// budget is durable. A "hard cap" (PRD US-012) that a restart resets isn't
// one; restarting a process is routine (redeploys, crash loops), not an
// exotic edge case to leave unguarded.
package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// BudgetWindow is the rolling period max_changes_per_24h is measured over.
const BudgetWindow = 24 * time.Hour

// changeEvent is one persisted record of an applied change — the minimal
// durable state ADR-013 requires: enough to reconstruct "how many changes
// has this disk had in the trailing 24h" on startup. Not a full decision
// journal (internal/journal, M5) — this file exists purely to keep the
// safety budget honest across a restart, and is compacted (below) to stay
// small, unlike the journal's full history.
type changeEvent struct {
	DiskID string    `json:"disk_id"`
	TS     time.Time `json:"ts"`
}

// Budget is the persisted 24h change-count record. Not safe for concurrent
// use on its own — Tracker (tracker.go) owns the locking; this type only
// owns getting bytes on disk without losing or corrupting them.
type Budget struct {
	path   string
	events []changeEvent // in-memory mirror, chronological; the file is the durable copy
}

// LoadBudget reads path (creating it if absent) and reconstructs every
// disk's change history. A line that fails to parse is skipped, not
// fatal — losing one historical event only makes the budget slightly more
// permissive for a moment; refusing to start the whole process over one
// corrupted line would be a worse failure mode than that.
func LoadBudget(path string) (*Budget, error) {
	b := &Budget{path: path}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("state: open budget file %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev changeEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		b.events = append(b.events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("state: read budget file %s: %w", path, err)
	}
	return b, nil
}

// CountSince returns how many changes diskID has had at or after cutoff.
func (b *Budget) CountSince(diskID string, cutoff time.Time) int {
	n := 0
	for _, ev := range b.events {
		if ev.DiskID == diskID && !ev.TS.Before(cutoff) {
			n++
		}
	}
	return n
}

// Record appends one change event, durably (including an fsync), before
// returning — a change Record hasn't returned from yet must never be lost
// to a crash immediately after the write it's recording actually landed.
func (b *Budget) Record(diskID string, ts time.Time) error {
	ev := changeEvent{DiskID: diskID, TS: ts}
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

// Compact rewrites the budget file keeping only events at or after cutoff
// — bounds the file's growth over a long-running process, which otherwise
// accumulates one line per change forever. Not called automatically; the
// caller decides the cadence (once per discovery cycle, design.md §4.1's
// slow path, is a natural fit — far less often than every reconcile).
// Writes to a temp file and renames over the original so a crash
// mid-compact never corrupts or truncates the durable record.
func (b *Budget) Compact(cutoff time.Time) error {
	kept := b.events[:0:0]
	for _, ev := range b.events {
		if !ev.TS.Before(cutoff) {
			kept = append(kept, ev)
		}
	}

	tmp := b.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("state: create compacted budget file: %w", err)
	}
	enc := json.NewEncoder(f)
	for _, ev := range kept {
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

	b.events = kept
	return nil
}
