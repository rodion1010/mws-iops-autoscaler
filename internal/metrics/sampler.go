package metrics

import (
	"context"
	"fmt"
	"sync"
)

// Sampler holds a fixed-size ring buffer of samples per disk (design.md
// §3.2: "Bounded memory; drops oldest"). It does not schedule itself — a
// later milestone's control loop calls Tick on collection_interval; Sampler
// itself is a pure, callable component, not a background goroutine.
type Sampler struct {
	source   MetricSource
	capacity int

	mu    sync.Mutex
	disks []DiskRef
	ring  map[string][]Sample // per DiskID, chronological, len <= capacity
}

// NewSampler builds a Sampler over source, keeping up to capacity samples
// per disk. capacity is the caller's responsibility to size — not simply
// the longest configured window divided by the collection interval: N
// samples at exact cadence span only (N-1) × interval end to end, so that
// naive division always comes up one sample short of ever spanning the
// window (issue #7). config.ResolvedConfig.SamplerCapacity is the
// canonical formula; the production orchestrator (cmd/mws-iops-autoscaler)
// always calls it rather than computing capacity itself.
func NewSampler(source MetricSource, capacity int) (*Sampler, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("sampler: capacity must be positive, got %d", capacity)
	}
	return &Sampler{
		source:   source,
		capacity: capacity,
		ring:     make(map[string][]Sample),
	}, nil
}

// SetDisks replaces the managed set — called after each discovery cycle
// (design.md §4.1 is the slow path; this is how its result reaches the
// sampler). Disks no longer present have their buffers dropped, not just
// stop being sampled: a disk that was detached and a different one
// reattached under the same alias must not silently inherit stale history.
func (s *Sampler) SetDisks(disks []DiskRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disks = append([]DiskRef(nil), disks...)

	keep := make(map[string]bool, len(disks))
	for _, d := range disks {
		keep[d.DiskID] = true
	}
	for id := range s.ring {
		if !keep[id] {
			delete(s.ring, id)
		}
	}
}

// Tick samples every managed disk once and appends the results to their
// ring buffers. A disk the source has no sample for this tick (cold start,
// unresolvable, counters went backwards) simply doesn't advance — never a
// reason to fail the whole tick.
func (s *Sampler) Tick(ctx context.Context) error {
	s.mu.Lock()
	disks := append([]DiskRef(nil), s.disks...)
	s.mu.Unlock()

	samples, err := s.source.Sample(ctx, disks)
	if err != nil {
		return fmt.Errorf("sampler tick: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sample := range samples {
		buf := append(s.ring[sample.DiskID], sample)
		if len(buf) > s.capacity {
			buf = buf[len(buf)-s.capacity:]
		}
		s.ring[sample.DiskID] = buf
	}
	return nil
}

// Samples returns a chronological copy of diskID's current buffer — a copy
// so the caller (the Analyzer, M3) can't mutate the Sampler's own state.
func (s *Sampler) Samples(diskID string) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.ring[diskID]...)
}
