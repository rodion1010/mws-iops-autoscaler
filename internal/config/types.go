// Package config parses and resolves the IOPS autoscaler's config file. Schema
// and validation rules are docs/configuration.md; the inheritance model is its
// §5. Phase 1 (docs/prd-mws-iops-autoscaler.md §6) is local mode only — fleet's
// project/instance layer is not implemented here yet.
package config

// Config is the top-level local-mode document (docs/configuration.md §6.1, §6.2).
type Config struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Mode       string      `yaml:"mode"`
	Policy     PolicySpec  `yaml:"policy"`
	Disks      DisksSpec   `yaml:"disks"`
	Runtime    RuntimeSpec `yaml:"runtime"`
}

// modePeek decodes just enough of the document to route it: local-mode
// documents are parsed strictly against Config; anything else is rejected
// before a field-by-field decode is attempted.
type modePeek struct {
	Mode string `yaml:"mode"`
}

// PolicySpec is a partial policy override. Every field is a pointer so the
// resolver (resolve.go) can tell "not set, inherit" apart from a zero value.
//
// Phase 1 ships one built-in default parameter set — no preset field. Named
// presets (balanced/performance/cost) are deferred; see ADR-006.
type PolicySpec struct {
	Limits   *LimitsSpec   `yaml:"limits"`
	Advanced *AdvancedSpec `yaml:"advanced"`
}

type LimitsSpec struct {
	MinIOPS *int `yaml:"min_iops"`
	MaxIOPS *int `yaml:"max_iops"`
}

// AdvancedSpec is opt-in (advanced.enabled must be true for any of it to take
// effect — docs/configuration.md §8) and API-only until telemetry justifies
// exposing it. billing_step_iops is deliberately absent: it is platform-fixed
// and not user-settable, so it is not a decodable field at all — an attempt
// to set it is rejected by strict decoding as an unknown key.
type AdvancedSpec struct {
	Enabled    *bool           `yaml:"enabled"`
	Evaluation *EvaluationSpec `yaml:"evaluation"`
	Saturation *SaturationSpec `yaml:"saturation"`
	Target     *TargetSpec     `yaml:"target"`
	Cooldown   *CooldownSpec   `yaml:"cooldown"`
	Step       *StepSpec       `yaml:"step"`
	Budget     *BudgetSpec     `yaml:"budget"`
}

type EvaluationSpec struct {
	CollectionInterval *Duration `yaml:"collection_interval"`
	ScaleUpWindow      *Duration `yaml:"scale_up_window"`
	ScaleDownWindow    *Duration `yaml:"scale_down_window"`
}

type SaturationSpec struct {
	// UtilizationThreshold gates on whichever of IOPS or bandwidth
	// utilization is higher, not IOPS alone (ADR-008).
	UtilizationThreshold *float64     `yaml:"utilization_threshold"`
	QueueDepthThreshold  *float64     `yaml:"queue_depth_threshold"`
	Latency              *LatencySpec `yaml:"latency"`
}

type LatencySpec struct {
	Mode               *string  `yaml:"mode"` // relative | absolute (absolute unimplemented)
	BaselineMultiplier *float64 `yaml:"baseline_multiplier"`
}

type TargetSpec struct {
	ScaleUpUtilization   *float64 `yaml:"scale_up_utilization"`
	ScaleDownUtilization *float64 `yaml:"scale_down_utilization"`
}

type CooldownSpec struct {
	ScaleUp   *Duration `yaml:"scale_up"`
	ScaleDown *Duration `yaml:"scale_down"`
}

type StepSpec struct {
	MinChangeIOPS     *int     `yaml:"min_change_iops"`
	MaxScaleUpRatio   *float64 `yaml:"max_scale_up_ratio"`
	MaxScaleDownRatio *float64 `yaml:"max_scale_down_ratio"`
}

type BudgetSpec struct {
	MaxChangesPer24h *int `yaml:"max_changes_per_24h"`
}

// DisksSpec is the disk selection strategy (docs/configuration.md §4).
type DisksSpec struct {
	Mode     string             `yaml:"mode"` // auto-discovery | selected
	Exclude  []string           `yaml:"exclude"`
	Selected []SelectedDiskSpec `yaml:"selected"`
}

// SelectedDiskSpec names one disk by device_name (ADR-004 — unprefixed, never
// a kernel device path) with an optional per-disk policy override.
type SelectedDiskSpec struct {
	DeviceName string      `yaml:"device_name"`
	Policy     *PolicySpec `yaml:"policy"`
}

type RuntimeSpec struct {
	Metrics    MetricsSpec    `yaml:"metrics"`
	Controller ControllerSpec `yaml:"controller"`
}

type MetricsSpec struct {
	Source             string    `yaml:"source"` // guest | platform
	CollectionInterval *Duration `yaml:"collection_interval"`
}

type ControllerSpec struct {
	ReconcileInterval *Duration `yaml:"reconcile_interval"`
	DiscoveryInterval *Duration `yaml:"discovery_interval"`
}
