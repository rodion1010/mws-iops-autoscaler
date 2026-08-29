package metrics

import (
	"context"
	"testing"
)

// fakeSource is a hand-written MetricSource for Sampler tests — no mocking
// framework, matching the rest of this codebase's testing style.
type fakeSource struct {
	next []Sample // returned once, then cleared, so each Tick controls its own batch
	err  error
}

func (f *fakeSource) Kind() SourceKind { return SourceGuest }

func (f *fakeSource) Sample(_ context.Context, _ []DiskRef) ([]Sample, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := f.next
	f.next = nil
	return out, nil
}

func TestNewSampler_rejectsNonPositiveCapacity(t *testing.T) {
	if _, err := NewSampler(&fakeSource{}, 0); err == nil {
		t.Fatal("NewSampler succeeded with capacity 0")
	}
	if _, err := NewSampler(&fakeSource{}, -1); err == nil {
		t.Fatal("NewSampler succeeded with negative capacity")
	}
}

func TestSampler_tickAppendsAndCapsAtCapacity(t *testing.T) {
	src := &fakeSource{}
	s, err := NewSampler(src, 3)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	s.SetDisks([]DiskRef{{DiskID: "disk-1"}})

	for i := 0; i < 5; i++ {
		src.next = []Sample{{DiskID: "disk-1", ReadOps: float64(i)}}
		if err := s.Tick(context.Background()); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}

	got := s.Samples("disk-1")
	if len(got) != 3 {
		t.Fatalf("len(Samples) = %d, want 3 (capped at capacity)", len(got))
	}
	// Oldest dropped: ticks 0 and 1 fell off, 2/3/4 remain, in order.
	for i, want := range []float64{2, 3, 4} {
		if got[i].ReadOps != want {
			t.Errorf("Samples[%d].ReadOps = %v, want %v", i, got[i].ReadOps, want)
		}
	}
}

func TestSampler_setDisksPrunesStaleBuffers(t *testing.T) {
	src := &fakeSource{}
	s, err := NewSampler(src, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	s.SetDisks([]DiskRef{{DiskID: "disk-1"}})
	src.next = []Sample{{DiskID: "disk-1", ReadOps: 1}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(s.Samples("disk-1")) != 1 {
		t.Fatal("expected one buffered sample before pruning")
	}

	// disk-1 is no longer managed as of the next discovery cycle.
	s.SetDisks([]DiskRef{{DiskID: "disk-2"}})
	if got := s.Samples("disk-1"); len(got) != 0 {
		t.Errorf("Samples(disk-1) after pruning = %d, want 0 — stale history must not survive", len(got))
	}
}

func TestSampler_tickPropagatesSourceError(t *testing.T) {
	src := &fakeSource{err: context.DeadlineExceeded}
	s, err := NewSampler(src, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("Tick succeeded despite the source returning an error")
	}
}

func TestSampler_samplesReturnsACopy(t *testing.T) {
	src := &fakeSource{}
	s, err := NewSampler(src, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	s.SetDisks([]DiskRef{{DiskID: "disk-1"}})
	src.next = []Sample{{DiskID: "disk-1", ReadOps: 1}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := s.Samples("disk-1")
	got[0].ReadOps = 999
	if s.Samples("disk-1")[0].ReadOps != 1 {
		t.Error("mutating the returned slice affected the Sampler's internal state")
	}
}
