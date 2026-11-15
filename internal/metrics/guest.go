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

// GuestSource is the `local`-mode MetricSource: the default source, needs no
// metrics credential, and reads /proc/diskstats directly.
//
// It is stateful: Sample computes a delta against the previous call, so the
// first call for a disk never yields a Sample.
type GuestSource struct {
	// procDiskstatsPath and resolveSymlink are overridable for tests, so
	// nothing here needs a real /proc or /dev/disk/by-id to run.
	procDiskstatsPath string
	resolveSymlink    func(path string) (string, error)
	now               func() time.Time

	mu       sync.Mutex
	prev     map[string]diskStats // keyed by DiskRef.IdentityKey()
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
	// Prune counters for disks that left the managed set before reading
	// /proc/diskstats, so pruning happens even when the read fails. Stale
	// counters would give a reattached disk a bogus delta. Keying by
	// IdentityKey() makes a recreated disk a new key that cold-starts.
	keep := make(map[string]bool, len(disks))
	for _, d := range disks {
		keep[d.IdentityKey()] = true
	}
	g.mu.Lock()
	for key := range g.prev {
		if !keep[key] {
			delete(g.prev, key)
			delete(g.prevTime, key)
		}
	}
	g.mu.Unlock()

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
			// An unresolvable disk is skipped, not fatal.
			continue
		}
		curr, ok := raw[kernelDevice]
		if !ok {
			continue
		}

		key := d.IdentityKey()
		prevStats, hadPrev := g.prev[key]
		prevTime := g.prevTime[key]
		g.prev[key] = curr
		g.prevTime[key] = now
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

// VerifyPath confirms d's guest device is resolvable and currently present
// in /proc/diskstats, the same checks Sample applies silently each cycle.
// Discovery uses it to reject an unmanageable disk up front with a specific
// reason instead of a later stream of undiagnosed "no data" cycles.
func (g *GuestSource) VerifyPath(d DiskRef) error {
	kernelDevice, err := g.resolveSymlink(d.GuestPath())
	if err != nil {
		return fmt.Errorf("resolve guest path %s: %w", d.GuestPath(), err)
	}

	f, err := os.Open(g.procDiskstatsPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", g.procDiskstatsPath, err)
	}
	defer f.Close()

	raw, err := parseDiskstats(f)
	if err != nil {
		return err
	}
	if _, ok := raw[kernelDevice]; !ok {
		return fmt.Errorf("kernel device %q (resolved from %s) has no entry in %s", kernelDevice, d.GuestPath(), g.procDiskstatsPath)
	}
	return nil
}

// resolveGuestSymlink follows /dev/disk/by-id/mws-<name> to its kernel
// device name, e.g. /dev/disk/by-id/mws-datadisk → sdb.
func resolveGuestSymlink(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return filepath.Base(resolved), nil
}
