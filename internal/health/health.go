// Package health implements design.md §7.3's two HTTP endpoints:
// /healthz (process alive) and /readyz (config valid, discovery succeeded,
// ≥1 metric source reachable, budget healthy). /readyz's body also reports
// any degraded disks (DegradedDisk) alongside that binary verdict, but
// never folds them into it — see SetDegradedDisks.
package health

import (
	"encoding/json"
	"net/http"
	"sync"
)

// DegradedDisk names one configured disk (disks.selected or disks.exclude)
// that could not be resolved against the VM's live disks this cycle, but
// did not block the rest of the managed set (docs/design.md §4.1's
// cold-start/runtime distinction; docs/configuration.md §4.2). DeviceName
// is whatever the operator's config named — it does not necessarily
// correspond to any live disk.
type DegradedDisk struct {
	DeviceName string `json:"device_name"`
	Reason     string `json:"reason"`
}

// Readiness is the mutable state /readyz reports on — set by the caller as
// each condition becomes true or false across the process's life. Zero
// value is "not ready" on every dimension, matching a freshly started
// process that hasn't discovered anything or reached a source yet.
type Readiness struct {
	mu                 sync.RWMutex
	configValid        bool
	discoverySucceeded bool
	sourceReachable    bool

	// budgetUnhealthy is inverted relative to the other three fields
	// (zero value = healthy) specifically so a freshly zero-valued
	// Readiness — including every existing `&Readiness{}` construction
	// site — starts healthy by default, matching state.Tracker's own
	// default, without requiring a constructor call (issue #10). The
	// exported surface (SetBudgetHealthy, the JSON body's BudgetHealthy
	// field) stays framed positively like every other field here; only
	// the internal storage is inverted.
	budgetUnhealthy bool

	// degraded is reported alongside the binary verdict below, never folded
	// into it (docs/design.md §4.1) — a disk named in disks.selected or
	// disks.exclude that didn't resolve degrades only that disk, once at
	// least one discovery has already succeeded; it never flips /readyz.
	degraded []DegradedDisk
}

func (r *Readiness) SetConfigValid(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configValid = v
}

func (r *Readiness) SetDiscoverySucceeded(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discoverySucceeded = v
}

func (r *Readiness) SetSourceReachable(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sourceReachable = v
}

// SetBudgetHealthy reports whether the durable 24h change budget can
// currently be trusted (issue #10) — false once any budget persistence
// operation (reserve/confirm/release) has failed, which state.Tracker
// treats as sticky for the process's lifetime. A process reporting
// unready for this reason needs a restart (which re-runs LoadBudget and
// startup reconciliation) to recover, not a wait — matching how the other
// three conditions here behave.
func (r *Readiness) SetBudgetHealthy(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budgetUnhealthy = !v
}

// SetDegradedDisks replaces the full current set of configured-but-
// unresolved disks reported alongside /readyz's binary verdict. The caller
// (Agent.discover) passes the complete set every cycle, including empty —
// that's what lets a disk that resolves again disappear from the body
// instead of leaving a stale entry behind.
func (r *Readiness) SetDegradedDisks(degraded []DegradedDisk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.degraded = degraded
}

// Ready reports whether every condition design.md §7.3 requires currently
// holds.
func (r *Readiness) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.configValid && r.discoverySucceeded && r.sourceReachable && !r.budgetUnhealthy
}

type readyzBody struct {
	Ready              bool `json:"ready"`
	ConfigValid        bool `json:"config_valid"`
	DiscoverySucceeded bool `json:"discovery_succeeded"`
	SourceReachable    bool `json:"source_reachable"`
	BudgetHealthy      bool `json:"budget_healthy"`

	// ConfiguredDisksUnmanaged is omitted entirely when empty, so the
	// healthy-state response stays byte-for-byte what it was before this
	// field existed.
	ConfiguredDisksUnmanaged []DegradedDisk `json:"configured_disks_unmanaged,omitempty"`
}

// ReadyzHandler returns 200 with a breakdown when every condition holds,
// 503 with the same breakdown otherwise — so an operator (or a load
// balancer's health-check log) can see *which* condition is failing, not
// just that something is.
func (r *Readiness) ReadyzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.RLock()
		body := readyzBody{
			Ready:                    r.configValid && r.discoverySucceeded && r.sourceReachable && !r.budgetUnhealthy,
			ConfigValid:              r.configValid,
			DiscoverySucceeded:       r.discoverySucceeded,
			SourceReachable:          r.sourceReachable,
			BudgetHealthy:            !r.budgetUnhealthy,
			ConfiguredDisksUnmanaged: r.degraded,
		}
		r.mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		if !body.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(body)
	})
}

// HealthzHandler always reports 200 while the process is up — "process
// alive" (design.md §7.3) is exactly "this handler got to run at all."
func HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}
