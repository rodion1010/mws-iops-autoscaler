package config

import "time"

// BillingStepIOPS is platform-fixed and not user-settable. It has no
// corresponding decodable field in AdvancedSpec.
const BillingStepIOPS = 100

// Platform per-disk IOPS bounds. The VM aggregate cap (3000 × vCPU) is not a
// config constant: it depends on the VM type and is resolved at discovery time.
const (
	PlatformMinIOPS = 1000
	PlatformMaxIOPS = 10000
)

// DefaultMinIOPS / DefaultMaxIOPS are used when policy.limits is omitted
// entirely.
const (
	DefaultMinIOPS = PlatformMinIOPS
	DefaultMaxIOPS = PlatformMaxIOPS
)

// ResolvedAdvanced is the advanced parameter set with nothing left to
// inherit. It has no CollectionInterval — that's ResolvedRuntime's alone:
// sampling cadence is process-wide, never per-policy.
type ResolvedAdvanced struct {
	ScaleUpWindow   time.Duration
	ScaleDownWindow time.Duration

	UtilizationThreshold      float64 // gates on IOPS or bandwidth, whichever is higher
	QueueDepthThreshold       float64
	LatencyMode               string
	LatencyBaselineMultiplier float64

	ScaleUpUtilization   float64
	ScaleDownUtilization float64

	CooldownScaleUp   time.Duration
	CooldownScaleDown time.Duration

	MinChangeIOPS     int
	MaxScaleUpRatio   float64
	MaxScaleDownRatio float64

	MaxChangesPer24h int
}

// defaultAdvanced is the built-in parameter set.
var defaultAdvanced = ResolvedAdvanced{
	ScaleUpWindow:             5 * time.Minute,
	ScaleDownWindow:           30 * time.Minute,
	UtilizationThreshold:      0.98,
	QueueDepthThreshold:       1,
	LatencyMode:               "relative",
	LatencyBaselineMultiplier: 1.5,
	ScaleUpUtilization:        0.85,
	ScaleDownUtilization:      0.70,
	CooldownScaleUp:           5 * time.Minute,
	CooldownScaleDown:         30 * time.Minute,
	MinChangeIOPS:             200,
	MaxScaleUpRatio:           2.0,
	MaxScaleDownRatio:         0.5,
	MaxChangesPer24h:          48,
}
