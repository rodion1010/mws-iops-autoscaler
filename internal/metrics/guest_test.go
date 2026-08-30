package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDiskstats(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "diskstats")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func newTestGuestSource(t *testing.T, path string, clock *time.Time) *GuestSource {
	t.Helper()
	g := NewGuestSource()
	g.procDiskstatsPath = path
	g.resolveSymlink = func(guestPath string) (string, error) {
		// datadisk resolves to sdb; all other names exercise the unresolved path.
		if guestPath == "/dev/disk/by-id/mws-datadisk" {
			return "sdb", nil
		}
		return "", os.ErrNotExist
	}
	g.now = func() time.Time { return *clock }
	return g
}

func TestGuestSource_coldStartYieldsNoSample(t *testing.T) {
	dir := t.TempDir()
	path := writeDiskstats(t, dir, "   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)

	disks := []DiskRef{{DiskID: "disk-1", DeviceName: "datadisk"}}
	samples, err := g.Sample(context.Background(), disks)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("first call returned %d samples, want 0 (cold start, no baseline yet)", len(samples))
	}
}

func TestGuestSource_secondCallYieldsDelta(t *testing.T) {
	dir := t.TempDir()
	path := writeDiskstats(t, dir, "   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)
	disks := []DiskRef{{DiskID: "disk-1", DeviceName: "datadisk"}}

	if _, err := g.Sample(context.Background(), disks); err != nil {
		t.Fatalf("first Sample: %v", err)
	}

	writeDiskstats(t, dir, "   8      16 sdb 1100 12 8800 600 2200 34 17600 1400 0 1500 8000\n")
	clock = time.Unix(10, 0)

	samples, err := g.Sample(context.Background(), disks)
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("len(samples) = %d, want 1", len(samples))
	}
	s := samples[0]
	if s.DiskID != "disk-1" || s.ReadOps != 10.0 || s.WriteOps != 20.0 {
		t.Errorf("unexpected sample: %+v", s)
	}
	if s.QueueDepth.Unit != QueueDepthOps || s.QueueDepth.Value != 0.5 {
		t.Errorf("QueueDepth = %+v, want {0.5 ops}", s.QueueDepth)
	}
}

func TestGuestSource_unresolvableDiskSkipped(t *testing.T) {
	dir := t.TempDir()
	path := writeDiskstats(t, dir, "   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)

	disks := []DiskRef{{DiskID: "disk-2", DeviceName: "ghost-disk"}}
	samples, err := g.Sample(context.Background(), disks)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("len(samples) = %d, want 0 (unresolvable disk skipped, not fatal)", len(samples))
	}
}

func TestGuestSource_deviceMissingFromDiskstatsSkipped(t *testing.T) {
	dir := t.TempDir()
	// diskstats contains no sdb entry.
	path := writeDiskstats(t, dir, "   8       0 sda 1 1 1 1 1 1 1 1 1 1 1\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)

	disks := []DiskRef{{DiskID: "disk-1", DeviceName: "datadisk"}}
	samples, err := g.Sample(context.Background(), disks)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("len(samples) = %d, want 0", len(samples))
	}
}

func TestGuestSource_kind(t *testing.T) {
	if NewGuestSource().Kind() != SourceGuest {
		t.Errorf("Kind() = %q, want %q", NewGuestSource().Kind(), SourceGuest)
	}
}

func TestVerifyPath_success(t *testing.T) {
	dir := t.TempDir()
	path := writeDiskstats(t, dir, "   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)

	if err := g.VerifyPath(DiskRef{DiskID: "disk-1", DeviceName: "datadisk"}); err != nil {
		t.Errorf("VerifyPath: %v", err)
	}
}

func TestVerifyPath_unresolvableSymlink(t *testing.T) {
	dir := t.TempDir()
	path := writeDiskstats(t, dir, "   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)

	err := g.VerifyPath(DiskRef{DiskID: "disk-2", DeviceName: "ghost-disk"})
	if err == nil {
		t.Fatal("VerifyPath succeeded for a device with no resolvable symlink (issue #4)")
	}
}

func TestVerifyPath_deviceMissingFromDiskstats(t *testing.T) {
	dir := t.TempDir()
	// The symlink resolves, but its kernel device is absent from diskstats.
	path := writeDiskstats(t, dir, "   8       0 sda 1 1 1 1 1 1 1 1 1 1 1\n")
	clock := time.Unix(0, 0)
	g := newTestGuestSource(t, path, &clock)

	err := g.VerifyPath(DiskRef{DiskID: "disk-1", DeviceName: "datadisk"})
	if err == nil {
		t.Fatal("VerifyPath succeeded for a kernel device missing from /proc/diskstats (issue #4)")
	}
}

func TestResolveGuestSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sdb")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "mws-datadisk")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	got, err := resolveGuestSymlink(link)
	if err != nil {
		t.Fatalf("resolveGuestSymlink: %v", err)
	}
	if got != "sdb" {
		t.Errorf("resolveGuestSymlink = %q, want sdb", got)
	}
}
