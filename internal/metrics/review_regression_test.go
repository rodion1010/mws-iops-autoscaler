package metrics

import (
	"context"
	"path/filepath"
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

// TestRegressionGuestSourcePrunesDepartedDiskEvenWhenDiskstatsReadFails is
// the second-review-pass finding: pruning a departed disk's counter
// baseline must not be gated on /proc/diskstats itself being readable this
// cycle. An earlier version of the fix pruned only after a successful
// open+parse, so a disk that left the managed set on a cycle where the
// read also happened to fail (a transient permissions glitch, the file
// momentarily missing) kept its stale baseline — a later reattach would
// then compute a delta against arbitrarily old counters instead of cold
// starting.
func TestRegressionGuestSourcePrunesDepartedDiskEvenWhenDiskstatsReadFails(t *testing.T) {
	dir := t.TempDir()
	path := writeDiskstats(t, dir, "   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)
	disk := DiskRef{DiskID: "disk-1", DeviceName: "datadisk", IncarnationID: "incarnation-a"}

	if _, err := g.Sample(context.Background(), []DiskRef{disk}); err != nil {
		t.Fatalf("initial Sample: %v", err)
	}

	// The disk leaves the managed set on a cycle where /proc/diskstats
	// itself is also unreadable — unrelated to the disk's own departure.
	g.procDiskstatsPath = filepath.Join(dir, "does-not-exist")
	clock = time.Unix(5, 0)
	if _, err := g.Sample(context.Background(), nil); err == nil {
		t.Fatal("test setup: expected the diskstats read to fail")
	}

	// Restore a readable file with new cumulative counters and reattach the
	// same disk — if the baseline had survived the failed read above, this
	// would compute a delta against the stale pre-departure counters
	// instead of cold-starting.
	g.procDiskstatsPath = writeDiskstats(t, dir, "   8      16 sdb 1100 12 8800 600 2200 34 17600 1400 0 1500 8000\n")
	clock = time.Unix(10, 0)
	samples, err := g.Sample(context.Background(), []DiskRef{disk})
	if err != nil {
		t.Fatalf("reattached Sample: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("reattached disk returned %d samples, want 0: a failed diskstats read must not block the prune for a disk that left the managed set", len(samples))
	}
}
