// Package metrics samples per-disk I/O statistics and buffers them for the
// Analyzer (M3). It is deliberately decoupled from internal/compute: the
// pipeline from here through the Actuator must stay pure and testable
// without touching the Compute API (design.md §8.1) — a future Disk
// Registry component converts compute.StorageDisk into DiskRef, not the
// other way around.
package metrics

import (
	"context"
	"time"
)

// SourceKind names which MetricSource produced a Sample.
type SourceKind string

const (
	SourceGuest    SourceKind = "guest"
	SourcePlatform SourceKind = "platform"
)

// QueueDepthUnit is explicit because the two sources are not comparable:
// guest gives operations, platform gives bytes (ADR-001). Carrying the unit
// on the value, not just in a comment, is deliberate — a silent mix would be
// a real and hard-to-spot bug (design.md §5.2).
type QueueDepthUnit string

const (
	QueueDepthOps   QueueDepthUnit = "ops"
	QueueDepthBytes QueueDepthUnit = "bytes"
)

type QueueDepth struct {
	Value float64
	Unit  QueueDepthUnit
}

// DiskRef is the identity of a managed disk, resolved once at discovery
// (design.md §5.2). DeviceName is the single identifier shared by config,
// guest and metrics (ADR-004): stored unprefixed, with the "mws-" prefix
// applied here when reading either side.
type DiskRef struct {
	DiskID     string // cloud resource name — used for API writes, never written in config
	VMName     string // (VMName, DeviceName) is the fleet-mode key
	DeviceName string // unprefixed: "datadisk"

	// IncarnationID is compute.StorageDisk.IncarnationID (metadata.id),
	// threaded through so the Sampler's own ring buffer can be keyed by
	// IdentityKey() rather than DiskID alone (pre-merge review: DiskName
	// is not a safe long-term identity — it can be reused by an
	// unrelated disk created after the original was deleted).
	IncarnationID string
}

// GuestPath is where this disk appears on the guest filesystem in `local`
// mode (docs/configuration.md §4.1).
func (d DiskRef) GuestPath() string {
	return "/dev/disk/by-id/mws-" + d.DeviceName
}

// MetricLabel is the device_name label value on this disk's exported
// metrics (docs/platform-reference.md, "Prometheus export").
func (d DiskRef) MetricLabel() string {
	return "mws-" + d.DeviceName
}

// IdentityKey is this DiskRef's long-term state identity — see
// compute.StorageDisk.IdentityKey's own comment for why DiskID alone is
// unsafe for this purpose. The Sampler uses this, not DiskID, as its ring
// buffer's map key.
func (d DiskRef) IdentityKey() string {
	return d.DiskID + "\x00" + d.IncarnationID
}

// Sample is one interval's observation — an average, not a percentile.
// Neither source can honestly give a within-interval percentile at the
// single-sample level: guest has only diskstats' cumulative ticks/ops, so
// an average is all it can ever produce; platform's real distribution
// lives in its exported histogram, not in any one Sample. The Analyzer
// (M3, US-007) turns these into a percentile over a window — the
// computation is deliberately *not* uniform across Source: guest always
// windows these averages, but platform prefers a real quantile computed
// directly from its histogram when one is available, falling back to the
// same windowed-average method otherwise. Which method backed a given
// percentile travels with the value (LatencyStatKind, design.md §5.2),
// the same move this package already makes for QueueDepth's unit — see
// ADR-011 for why forcing both sources through one uniform, coarser
// method was the wrong fix for that risk.
type Sample struct {
	DiskID      string
	Timestamp   time.Time // metric timestamp, not wall clock
	ReadOps     float64   // ops/s
	WriteOps    float64   // ops/s
	ReadBytes   float64   // bytes/s
	WriteBytes  float64   // bytes/s
	QueueDepth  QueueDepth
	ReadLatAvg  time.Duration // average latency this interval, not a percentile
	WriteLatAvg time.Duration
	Source      SourceKind
}

// MetricSource is implemented by GuestSource (this package) and, in Phase 2
// (PRD §6), a platform source. A source may return fewer samples than disks
// requested — a disk with no prior reading yet (cold start) or that failed
// to resolve is simply absent from the result, not an error.
type MetricSource interface {
	Sample(ctx context.Context, disks []DiskRef) ([]Sample, error)
	Kind() SourceKind
}
