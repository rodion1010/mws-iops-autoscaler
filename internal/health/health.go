// Package health implements design.md §7.3's two HTTP endpoints:
// /healthz (process alive) and /readyz (config valid, discovery succeeded,
// ≥1 metric source reachable).
package health

import (
	"encoding/json"
	"net/http"
	"sync"
)

// Readiness is the mutable state /readyz reports on — set by the caller as
// each condition becomes true or false across the process's life. Zero
// value is "not ready" on every dimension, matching a freshly started
// process that hasn't discovered anything or reached a source yet.
type Readiness struct {
	mu                 sync.RWMutex
	configValid        bool
	discoverySucceeded bool
	sourceReachable    bool
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

// Ready reports whether every condition design.md §7.3 requires currently
// holds.
func (r *Readiness) Ready() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.configValid && r.discoverySucceeded && r.sourceReachable
}

type readyzBody struct {
	Ready              bool `json:"ready"`
	ConfigValid        bool `json:"config_valid"`
	DiscoverySucceeded bool `json:"discovery_succeeded"`
	SourceReachable    bool `json:"source_reachable"`
}

// ReadyzHandler returns 200 with a breakdown when every condition holds,
// 503 with the same breakdown otherwise — so an operator (or a load
// balancer's health-check log) can see *which* condition is failing, not
// just that something is.
func (r *Readiness) ReadyzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.RLock()
		body := readyzBody{
			Ready:              r.configValid && r.discoverySucceeded && r.sourceReachable,
			ConfigValid:        r.configValid,
			DiscoverySucceeded: r.discoverySucceeded,
			SourceReachable:    r.sourceReachable,
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
