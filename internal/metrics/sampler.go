package metrics

import (
	"context"
	"fmt"
	"sync"
)

// Sampler holds a fixed-size ring buffer of samples per disk; when full it
// drops the oldest. It does not schedule itself: the caller invokes Tick on
// collection_interval.
type Sampler struct {
	source   MetricSource
	capacity int

	mu    sync.Mutex
	disks []DiskRef
	ring  map[string][]Sample // per DiskRef.IdentityKey(), chronological, len <= capacity
}

// NewSampler builds a Sampler over source, keeping up to capacity samples
// per disk. It returns an error when capacity is not positive. N samples at
// exact cadence span only (N-1) × interval, so window/interval is one sample
// short; size capacity with config.ResolvedConfig.SamplerCapacity.
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

// SetDisks replaces the managed set after each discovery cycle. Buffers of
// disks no longer present are dropped, so a disk reattached under the same
// alias, or deleted and recreated under the same DiskName, does not inherit
// stale history: buffers are keyed by IdentityKey(), which includes the
// IncarnationID.
func (s *Sampler) SetDisks(disks []DiskRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disks = append([]DiskRef(nil), disks...)

	keep := make(map[string]bool, len(disks))
	for _, d := range disks {
		keep[d.IdentityKey()] = true
	}
	for key := range s.ring {
		if !keep[key] {
			delete(s.ring, key)
		}
	}
}

// Tick samples every managed disk once and appends the results to their
// ring buffers. A disk without a sample this tick (cold start, unresolvable,
// counters went backwards) does not advance; only a source-wide error fails
// the tick.
func (s *Sampler) Tick(ctx context.Context) error {
	s.mu.Lock()
	disks := append([]DiskRef(nil), s.disks...)
	s.mu.Unlock()

	samples, err := s.source.Sample(ctx, disks)
	if err != nil {
		return fmt.Errorf("sampler tick: %w", err)
	}

	// Sources match samples by DiskID only; this lookup recovers the full
	// identity, including IncarnationID, for the ring buffer key.
	byDiskID := make(map[string]DiskRef, len(disks))
	for _, d := range disks {
		byDiskID[d.DiskID] = d
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sample := range samples {
		ref, ok := byDiskID[sample.DiskID]
		if !ok {
			// A sample for a disk outside the request set has no safe buffer.
			continue
		}
		key := ref.IdentityKey()
		buf := append(s.ring[key], sample)
		if len(buf) > s.capacity {
			buf = buf[len(buf)-s.capacity:]
		}
		s.ring[key] = buf
	}
	return nil
}

// Samples returns a chronological copy of ref's buffer, keyed by
// ref.IdentityKey(), so the caller cannot mutate the Sampler's state.
func (s *Sampler) Samples(ref DiskRef) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.ring[ref.IdentityKey()]...)
}
