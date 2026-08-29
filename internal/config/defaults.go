package config

import "time"

// BillingStepIOPS is platform-fixed and not user-settable (docs/configuration.md §2
// field notes). It has no corresponding decodable field in AdvancedSpec.
const BillingStepIOPS = 100

// Platform bounds — CLAUDE.md "IOPS limits". The VM aggregate cap (3000 × vCPU)
// is not a config constant: it depends on the VM type and is resolved at
// discovery time (design.md §4.1), not here.
const (
	PlatformMinIOPS = 1000
	PlatformMaxIOPS = 10000
)

// DefaultMinIOPS / DefaultMaxIOPS are docs/prd-mws-iops-autoscaler.md §7 defaults,
// used when policy.limits is omitted entirely.
const (
	DefaultMinIOPS = PlatformMinIOPS
	DefaultMaxIOPS = PlatformMaxIOPS
)

// ResolvedAdvanced is the advanced parameter set with nothing left to inherit.
type ResolvedAdvanced struct {
	CollectionInterval time.Duration
	ScaleUpWindow      time.Duration
	ScaleDownWindow    time.Duration

	IOPSUtilizationThreshold  float64
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

// defaultAdvanced is Phase 1's one built-in parameter set — formerly the
// "balanced" preset (docs/configuration.md §3). Named presets
// (balanced/performance/cost) are deferred past the MVP: implementing the
// cross-field validation in validate.go found that "performance"'s own
// values (2m scale_up_window, 3m cooldown.scale_up) violated the very rule
// that set of numbers is supposed to satisfy. Rather than keep three preset
// tables mutually consistent under every future rule, Phase 1 ships one
// validated set and re-introduces presets when there is a reason to choose
// between them (ADR-006).
var defaultAdvanced = ResolvedAdvanced{
	CollectionInterval:        10 * time.Second,
	ScaleUpWindow:             5 * time.Minute,
	ScaleDownWindow:           30 * time.Minute,
	IOPSUtilizationThreshold:  0.98,
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
