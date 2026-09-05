package metrics

import (
	"context"
	"testing"
)

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
	ref := DiskRef{DiskID: "disk-1"}
	s.SetDisks([]DiskRef{ref})

	for i := 0; i < 5; i++ {
		src.next = []Sample{{DiskID: "disk-1", ReadOps: float64(i)}}
		if err := s.Tick(context.Background()); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}

	got := s.Samples(ref)
	if len(got) != 3 {
		t.Fatalf("len(Samples) = %d, want 3 (capped at capacity)", len(got))
	}
	// Capacity three retains ticks 2, 3, and 4 in order.
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
	ref1 := DiskRef{DiskID: "disk-1"}
	s.SetDisks([]DiskRef{ref1})
	src.next = []Sample{{DiskID: "disk-1", ReadOps: 1}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(s.Samples(ref1)) != 1 {
		t.Fatal("expected one buffered sample before pruning")
	}

	// disk-1 leaves the managed set at the next discovery.
	s.SetDisks([]DiskRef{{DiskID: "disk-2"}})
	if got := s.Samples(ref1); len(got) != 0 {
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
	ref := DiskRef{DiskID: "disk-1"}
	s.SetDisks([]DiskRef{ref})
	src.next = []Sample{{DiskID: "disk-1", ReadOps: 1}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := s.Samples(ref)
	got[0].ReadOps = 999
	if s.Samples(ref)[0].ReadOps != 1 {
		t.Error("mutating the returned slice affected the Sampler's internal state")
	}
}

func TestSampler_recreatedDiskUnderSameDiskIDStartsEmpty(t *testing.T) {
	src := &fakeSource{}
	s, err := NewSampler(src, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}

	original := DiskRef{DiskID: "vm-1-data", IncarnationID: "incarnation-1"}
	s.SetDisks([]DiskRef{original})
	src.next = []Sample{{DiskID: "vm-1-data", ReadOps: 500}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(s.Samples(original)) != 1 {
		t.Fatal("test setup: expected one buffered sample for the original incarnation")
	}

	// Discovery now returns the same name with a new incarnation.
	recreated := DiskRef{DiskID: "vm-1-data", IncarnationID: "incarnation-2"}
	s.SetDisks([]DiskRef{recreated})

	if got := s.Samples(recreated); len(got) != 0 {
		t.Errorf("Samples(recreated) = %d, want 0 — a new incarnation must never inherit the previous one's buffered history", len(got))
	}
	if got := s.Samples(original); len(got) != 0 {
		t.Errorf("Samples(original) after the incarnation changed = %d, want 0 — the old incarnation's buffer must be dropped, not just shadowed", len(got))
	}
}

func TestSampler_tickRoutesSamplesToTheCurrentIncarnation(t *testing.T) {
	src := &fakeSource{}
	s, err := NewSampler(src, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}

	recreated := DiskRef{DiskID: "vm-1-data", IncarnationID: "incarnation-2"}
	s.SetDisks([]DiskRef{recreated})
	src.next = []Sample{{DiskID: "vm-1-data", ReadOps: 42}}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := s.Samples(recreated)
	if len(got) != 1 || got[0].ReadOps != 42 {
		t.Errorf("Samples(recreated) = %+v, want the one sample just ticked", got)
	}
}
