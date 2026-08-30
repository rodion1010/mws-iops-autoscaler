package metrics

import (
	"context"
	"testing"
	"time"
)

func TestRegressionGuestSourceColdStartsEachDiskIncarnation(t *testing.T) {
	dir := t.TempDir()
	path := writeDiskstats(t, dir, "   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)
	original := DiskRef{DiskID: "disk-1", DeviceName: "datadisk", IncarnationID: "incarnation-a"}
	replacement := DiskRef{DiskID: "disk-1", DeviceName: "datadisk", IncarnationID: "incarnation-b"}

	if samples, err := g.Sample(context.Background(), []DiskRef{original}); err != nil {
		t.Fatalf("original Sample: %v", err)
	} else if len(samples) != 0 {
		t.Fatalf("original cold start returned %d samples, want 0", len(samples))
	}

	writeDiskstats(t, dir, "   8      16 sdb 1100 12 8800 600 2200 34 17600 1400 0 1500 8000\n")
	clock = time.Unix(10, 0)
	samples, err := g.Sample(context.Background(), []DiskRef{replacement})
	if err != nil {
		t.Fatalf("replacement Sample: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("replacement's first observation returned %d samples, want 0: it must not compute a delta from incarnation-a", len(samples))
	}
}

func TestRegressionGuestSourceDropsStateForDisappearedDisk(t *testing.T) {
	dir := t.TempDir()
	path := writeDiskstats(t, dir, "   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)
	disk := DiskRef{DiskID: "disk-1", DeviceName: "datadisk", IncarnationID: "incarnation-a"}

	if _, err := g.Sample(context.Background(), []DiskRef{disk}); err != nil {
		t.Fatalf("initial Sample: %v", err)
	}
	clock = time.Unix(5, 0)
	if _, err := g.Sample(context.Background(), nil); err != nil {
		t.Fatalf("empty managed-set Sample: %v", err)
	}

	writeDiskstats(t, dir, "   8      16 sdb 1100 12 8800 600 2200 34 17600 1400 0 1500 8000\n")
	clock = time.Unix(10, 0)
	samples, err := g.Sample(context.Background(), []DiskRef{disk})
	if err != nil {
		t.Fatalf("reattached Sample: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("reattached disk returned %d samples, want 0: disappearance must discard its old counter baseline", len(samples))
	}
}
