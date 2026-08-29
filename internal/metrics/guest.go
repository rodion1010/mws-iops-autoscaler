package metrics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const defaultProcDiskstatsPath = "/proc/diskstats"

// GuestSource is the `local`-mode MetricSource: primary by default (ADR-001),
// needs no metrics credential, reads /proc/diskstats directly.
//
// It is stateful — Sample computes a delta against the previous call, so the
// first call for a disk never yields a Sample (cold start, no baseline yet;
// design.md §4.3's "window_not_full skip" principle applies here too, one
// level earlier).
type GuestSource struct {
	// procDiskstatsPath and resolveSymlink are overridable for tests, so
	// nothing here needs a real /proc or /dev/disk/by-id to run.
	procDiskstatsPath string
	resolveSymlink    func(path string) (string, error)
	now               func() time.Time

	mu       sync.Mutex
	prev     map[string]diskStats // keyed by DiskRef.DiskID
	prevTime map[string]time.Time
}

func NewGuestSource() *GuestSource {
	return &GuestSource{
		procDiskstatsPath: defaultProcDiskstatsPath,
		resolveSymlink:    resolveGuestSymlink,
		now:               time.Now,
		prev:              make(map[string]diskStats),
		prevTime:          make(map[string]time.Time),
	}
}

func (g *GuestSource) Kind() SourceKind { return SourceGuest }

func (g *GuestSource) Sample(_ context.Context, disks []DiskRef) ([]Sample, error) {
	f, err := os.Open(g.procDiskstatsPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", g.procDiskstatsPath, err)
	}
	defer f.Close()

	raw, err := parseDiskstats(f)
	if err != nil {
		return nil, err
	}
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()

	var out []Sample
	for _, d := range disks {
		kernelDevice, err := g.resolveSymlink(d.GuestPath())
		if err != nil {
			// Unresolvable disk: skipped, not fatal (US-002's same
			// principle applied to sampling, not just discovery).
			continue
		}
		curr, ok := raw[kernelDevice]
		if !ok {
			continue
		}

		prevStats, hadPrev := g.prev[d.DiskID]
		prevTime := g.prevTime[d.DiskID]
		g.prev[d.DiskID] = curr
		g.prevTime[d.DiskID] = now
		if !hadPrev {
			continue
		}

		sample, ok := computeSample(d.DiskID, prevStats, curr, now.Sub(prevTime), now)
		if !ok {
			continue
		}
		out = append(out, sample)
	}
	return out, nil
}

// resolveGuestSymlink follows /dev/disk/by-id/mws-<name> to its kernel
// device name (e.g. "sdb") — confirmed live in CLAUDE.md's test environment
// ("/dev/disk/by-id/mws-datadisk → sdb").
func resolveGuestSymlink(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return filepath.Base(resolved), nil
}
