package config

import "time"

// BillingStepIOPS is platform-fixed and not user-settable (docs/configuration.md §2
// field notes). It has no corresponding decodable field in AdvancedSpec.
const BillingStepIOPS = 100

// Platform bounds — docs/platform-reference.md "IOPS limits". The VM aggregate cap (3000 × vCPU)
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

// ResolvedAdvanced is the advanced parameter set with nothing left to
// inherit. It has no CollectionInterval — that's ResolvedRuntime's alone
// (ADR-010): sampling cadence is process-wide, never per-policy.
type ResolvedAdvanced struct {
	ScaleUpWindow   time.Duration
	ScaleDownWindow time.Duration

	UtilizationThreshold      float64 // gates on IOPS or bandwidth, whichever is higher (ADR-008)
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

// PresetName identifies one entry in the preset registry below. Not a
// decodable YAML field yet — Phase 1's schema has no `preset:` key, so
// nothing in this package resolves a PresetName from config. The type and
// the registry exist now so that exposing presets in Phase 5 is additive
// (a field, a name→ResolvedAdvanced lookup, and a validation rule) rather
// than a rework of the resolver. See ADR-006's Phase 5 amendment.
type PresetName string

const (
	PresetBalanced    PresetName = "balanced" // Phase 1's one built-in default
	PresetPerformance PresetName = "performance"
	PresetCost        PresetName = "cost"
)

// DefaultPresetName is what defaultResolvedPolicy resolves to today — the
// only entry Phase 1 actually uses.
const DefaultPresetName = PresetBalanced

// presets is the full registry, transcribed from docs/configuration.md §3
// and ADR-006 (performance's values there are already corrected — see that
// ADR for why the first draft's 2m/3m pair was wrong). Phase 1 reads only
// presets[DefaultPresetName]; performance and cost sit here unused until
// Phase 5 exposes a way to select them.
var presets = map[PresetName]ResolvedAdvanced{
	PresetBalanced: {
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
	},
	PresetPerformance: {
		ScaleUpWindow:             3 * time.Minute, // ADR-006: first draft had 2m, violated §8's cooldown rule
		ScaleDownWindow:           60 * time.Minute,
		UtilizationThreshold:      0.95,
		QueueDepthThreshold:       1,
		LatencyMode:               "relative",
		LatencyBaselineMultiplier: 1.25,
		ScaleUpUtilization:        0.75,
		ScaleDownUtilization:      0.60,
		CooldownScaleUp:           3 * time.Minute,
		CooldownScaleDown:         60 * time.Minute,
		MinChangeIOPS:             200,
		MaxScaleUpRatio:           2.0,
		MaxScaleDownRatio:         0.75,
		MaxChangesPer24h:          48,
	},
	PresetCost: {
		ScaleUpWindow:             10 * time.Minute,
		ScaleDownWindow:           20 * time.Minute,
		UtilizationThreshold:      1.00,
		QueueDepthThreshold:       2,
		LatencyMode:               "relative",
		LatencyBaselineMultiplier: 2.0,
		ScaleUpUtilization:        0.90,
		ScaleDownUtilization:      0.80,
		CooldownScaleUp:           10 * time.Minute,
		CooldownScaleDown:         20 * time.Minute,
		MinChangeIOPS:             300,
		MaxScaleUpRatio:           1.5,
		MaxScaleDownRatio:         0.5,
		MaxChangesPer24h:          48,
	},
}

// defaultAdvanced is Phase 1's one built-in parameter set — presets[DefaultPresetName].
// Named presets (performance/cost) are laid out above but not selectable
// until Phase 5 (ADR-006): implementing the cross-field validation in
// validate.go against the first-draft preset table found "performance"'s
// own values (2m scale_up_window, 3m cooldown.scale_up) violated the very
// rule that set of numbers is supposed to satisfy. Rather than keep three
// preset tables mutually consistent under every future rule this early,
// Phase 1 resolves against one validated set.
var defaultAdvanced = presets[DefaultPresetName]
