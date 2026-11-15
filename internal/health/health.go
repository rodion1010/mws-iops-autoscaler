// Package health serves /healthz (process alive) and /readyz (config valid,
// discovery succeeded, ≥1 metric source reachable, budget healthy). /readyz
// also lists degraded disks, which never affect the readiness verdict.
package health

import (
	"encoding/json"
	"net/http"
	"sync"
)

// DegradedDisk names one configured disk (disks.policies or disks.exclude)
// that could not be resolved against the VM's live disks this cycle but did
// not block the rest of the managed set. DeviceName is the name from the
// config and need not match any live disk.
type DegradedDisk struct {
	DeviceName string `json:"device_name"`
	Reason     string `json:"reason"`
}

// Readiness is the mutable state /readyz reports on; it is safe for
// concurrent use. The zero value is not ready: config, discovery and source
// are false, while the budget starts healthy.
type Readiness struct {
	mu                 sync.RWMutex
	configValid        bool
	discoverySucceeded bool
	sourceReachable    bool

	// budgetUnhealthy is stored inverted so a zero-value Readiness starts
	// budget-healthy, matching state.Tracker's default. The exported surface
	// stays positive (SetBudgetHealthy, BudgetHealthy).
	budgetUnhealthy bool

	// degraded is reported alongside the verdict, never folded into it: an
	// unresolved configured disk degrades only that disk and never flips
	// /readyz.
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

// SetBudgetHealthy records whether the durable 24h change budget can be
// trusted. state.Tracker reports false, stickily for the process lifetime,
// once any budget persistence operation fails; recovery needs a restart,
// which reloads the budget and reruns startup reconciliation.
func (r *Readiness) SetBudgetHealthy(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budgetUnhealthy = !v
}

// SetDegradedDisks replaces the full current set of configured-but-
// unresolved disks reported by /readyz. Callers pass the complete set every
// cycle, including an empty one, so a disk that resolves again leaves no
// stale entry.
func (r *Readiness) SetDegradedDisks(degraded []DegradedDisk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.degraded = degraded
}

// Ready reports whether every readiness condition currently holds.
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

	// ConfiguredDisksUnmanaged is omitted when empty.
	ConfiguredDisksUnmanaged []DegradedDisk `json:"configured_disks_unmanaged,omitempty"`
}

// ReadyzHandler returns 200 with a breakdown when every condition holds,
// 503 with the same breakdown otherwise, so an operator can see which
// condition is failing.
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

// HealthzHandler always reports 200: serving the request is the liveness
// signal.
func HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}
