// Package metrics samples and buffers per-disk I/O observations without depending on Compute.
package metrics

import (
	"context"
	"time"
)

// SourceKind identifies the MetricSource that produced a sample.
type SourceKind string

const (
	SourceGuest    SourceKind = "guest"
	SourcePlatform SourceKind = "platform"
)

// QueueDepthUnit prevents ops- and byte-denominated queue values from being mixed.
type QueueDepthUnit string

const (
	QueueDepthOps   QueueDepthUnit = "ops"
	QueueDepthBytes QueueDepthUnit = "bytes"
)

type QueueDepth struct {
	Value float64
	Unit  QueueDepthUnit
}

// DiskRef is the discovery-resolved identity needed by metric sources and buffering.
type DiskRef struct {
	DiskID     string // cloud resource name — used for API writes, never written in config
	VMName     string // (VMName, DeviceName) is the fleet-mode key
	DeviceName string // the exact value VM status reports: "mws-datadisk"

	// IncarnationID prevents a replacement disk from inheriting the previous ring buffer.
	IncarnationID string
}

// GuestPath is the local-mode device path.
func (d DiskRef) GuestPath() string {
	return "/dev/disk/by-id/" + d.DeviceName
}

// MetricLabel is the exported device_name label.
func (d DiskRef) MetricLabel() string {
	return d.DeviceName
}

// IdentityKey is the incarnation-aware sampler key.
func (d DiskRef) IdentityKey() string {
	return d.DiskID + "\x00" + d.IncarnationID
}

// Sample is one interval average, not a percentile.
// Analyzer derives window statistics and records how each latency value was produced.
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

// MetricSource may omit disks with no observation; source-wide failures return an error.
type MetricSource interface {
	Sample(ctx context.Context, disks []DiskRef) ([]Sample, error)
	Kind() SourceKind
}
