// Package state is the durable+in-memory state Guardrails needs across
// reconcile cycles and process restarts (docs/design.md §9.4): cooldown timestamps stay
// in memory — safe, because internal/config's validation enforces
// cooldown <= its paired window, so the cold-start wait after a restart is
// never shorter than the cooldown a restart forgot — but the 24h change
// budget is durable. A "hard cap" (PRD US-012) that a restart resets isn't
// one; restarting a process is routine (redeploys, crash loops), not an
// exotic edge case to leave unguarded.
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

// BudgetWindow is the rolling period max_changes_per_24h is measured over.
const BudgetWindow = 24 * time.Hour

// Status is a ChangeEvent's place in its own intent/result state machine
// (issue #10, reopened). A reservation counts against the budget the
// moment it is made — before the write is even attempted — precisely so a
// crash between the platform confirming a write and this process
// recording that fact can never silently undercount a real change against
// the hard cap. Only a conclusively-verified non-write releases it.
type Status string

const (
	// StatusPending is set the instant a change is about to be attempted.
	// Counts against the budget. Must be resolved (Confirmed or Released)
	// before this process trusts the budget again after a restart.
	StatusPending Status = "pending"
	// StatusConfirmed is set once the write has been verified applied.
	// Still counts against the budget — identical to StatusPending for
	// counting purposes, since both represent a change that happened.
	StatusConfirmed Status = "confirmed"
	// StatusReleased is set only once live disk state has been verified
	// to still match the reservation's own Before value — the write
	// conclusively never landed. The only status that stops counting.
	StatusReleased Status = "released"
)

// ChangeEvent is one persisted record in the budget's append-only
// intent/result log — the minimal durable state docs/design.md §9.4/issue #10
// require: enough to reconstruct "how many changes has this disk had in
// the trailing 24h" on startup, and enough to reconcile an in-flight
// change against live state after a crash. Not a full decision journal
// (internal/journal, M5) — this file exists purely to keep the safety
// budget honest, and is compacted (below) to stay small.
//
// A logical reservation is one or more lines sharing the same
// IdempotencyKey: the first (Reserve) carries every field; a later
// transition (Confirm/Release) re-appends the same DiskID/TS/Before/Target
// with only Status changed, so each line is self-contained and a reader
// never needs to join records across lines beyond grouping by key.
type ChangeEvent struct {
	DiskID         string    `json:"disk_id"`
	TS             time.Time `json:"ts"` // reservation time — fixed across all of a key's transitions; budget counts from here
	IdempotencyKey string    `json:"idempotency_key"`
	Before         int64     `json:"before"` // Decision.Current at reservation time — the value a released write must still match
	Target         int64     `json:"target"` // Decision.Target — the value a confirmed write must match
	Status         Status    `json:"status"`
}

// Budget is the persisted 24h change-budget record. Not safe for
// concurrent use on its own — Tracker (tracker.go) owns the locking for
// its own in-memory state, and this package's only caller
// (cmd/mws-iops-autoscaler.Agent) drives discover/reconcile from a single
// goroutine's select loop, never concurrently with itself — so Budget
// itself carries no mutex. This type only owns getting bytes on disk
// without losing or corrupting them.
type Budget struct {
	path   string
	events []ChangeEvent // in-memory mirror, append order; the file is the durable copy
}

// LoadBudget reads path (creating it if absent) and reconstructs every
// disk's change history.
//
// A line that fails to parse is handled two different ways depending on
// where it is (issue #10, reopened — the previous behavior silently
// skipped every corrupt line anywhere in the file, which is fail-open: it
// can only ever make the budget look more permissive than it should,
// never less). A corrupt *last* line is treated as a torn write — the
// ordinary signature of a crash mid-append (the fsync in append() means a
// torn write can only ever be the final, incomplete line, never one in
// the middle) — and is safely dropped, durably, before LoadBudget
// returns. A corrupt line anywhere else indicates the file itself is
// unreliable in a way a torn-tail recovery can't explain, and LoadBudget
// fails outright: refusing to start with a change budget that might be
// silently wrong is the fail-closed choice for a safety invariant.
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
		// Durably drop the torn line by rewriting the file from the
		// events successfully parsed so far — never leave the torn bytes
		// in place for the next append to land after.
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

// latestByKey collapses the append-only log to one entry per idempotency
// key — later transitions (Confirm/Release) only ever append a new line
// for a key already seen, so the last line for a given key is always its
// current, authoritative status.
func (b *Budget) latestByKey() map[string]ChangeEvent {
	latest := make(map[string]ChangeEvent, len(b.events))
	for _, ev := range b.events {
		latest[ev.IdempotencyKey] = ev
	}
	return latest
}

// CountSince returns how many of diskID's changes at or after cutoff are
// still counted against the budget — every reservation whose latest known
// status is not Released, Pending included (issue #10: a change that
// crashed before this process resolved it must keep consuming budget
// until reconciliation conclusively proves it never landed, not simply
// stop counting because nothing said otherwise).
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

// Pending returns every reservation whose latest known status is still
// StatusPending — the set a caller must reconcile against live disk state
// before trusting the budget again, typically once at startup right after
// the first successful discovery (issue #10).
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

// Reserve durably records that diskID's change is about to be attempted —
// called before the write itself (issue #10, reopened). The previous
// design recorded a change only after a successful write returned,
// leaving a gap where a crash between the platform confirming the write
// and this process persisting that fact could silently undercount a real
// change against the hard cap. Reserving first closes that gap: the
// budget slot is consumed the instant the attempt begins, and a write
// that turns out to have failed is explicitly Released, never simply
// left unrecorded.
func (b *Budget) Reserve(diskID, idempotencyKey string, before, target int64, ts time.Time) error {
	if idempotencyKey == "" {
		return fmt.Errorf("state: reserve budget slot for disk %q: empty idempotency key", diskID)
	}
	return b.append(ChangeEvent{
		DiskID: diskID, TS: ts, IdempotencyKey: idempotencyKey,
		Before: before, Target: target, Status: StatusPending,
	})
}

// Confirm marks idempotencyKey's reservation as a verified, applied
// change. A no-op on the budget count itself (Reserve already counted
// it) — it exists so a future reconciliation never has to re-derive the
// outcome from live state again once it's already known.
func (b *Budget) Confirm(idempotencyKey string) error {
	return b.transition(idempotencyKey, StatusConfirmed)
}

// Release marks idempotencyKey's reservation as conclusively not applied
// — the only transition that stops it counting against the budget.
// Callers must only call this once live disk state has been verified to
// still match the reservation's own Before value (issue #10); releasing
// on a merely-failed write attempt without that verification would
// reopen the exact undercounting gap Reserve-before-write exists to
// close — a write can fail locally (network error, etc.) while having
// already landed on the platform.
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

// append durably records ev, including an fsync, before returning — an
// append that hasn't returned yet must never be lost to a crash
// immediately after the write it's recording actually landed.
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

// Compact rewrites the budget file keeping every still-pending
// reservation regardless of age (issue #10: a pending entry must survive
// compaction until it's been conclusively reconciled — discarding it
// early would silently undo the very guarantee Reserve-before-write
// exists to provide) plus every resolved (confirmed/released) event at or
// after cutoff. Bounds the file's growth over a long-running process,
// which otherwise accumulates one line per change (and per transition)
// forever. Not called automatically; the caller decides the cadence (once
// per discovery cycle, design.md §4.1's slow path, is a natural fit — far
// less often than every reconcile). Writes to a temp file and renames
// over the original, then fsyncs the containing directory, so a crash
// mid-compact — or immediately after the rename, before the directory
// entry itself is durable — never corrupts or loses the durable record
// (issue #10's "fsync the containing directory after rename").
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

// writeAll atomically replaces the budget file's contents with events —
// shared by Compact and LoadBudget's torn-tail recovery, so both go
// through the identical temp-file-then-rename-then-fsync-directory path.
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

// syncDir fsyncs the directory containing path — required on POSIX for a
// rename (or a new file's creation) to be durable across a crash: the
// rename/create itself is atomic, but the directory entry update isn't
// guaranteed on disk until the directory's own fsync (issue #10).
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
