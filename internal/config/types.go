// Package config parses, validates and resolves the IOPS autoscaler's config
// file. Only local mode is supported; fleet mode's project/instance layer is
// not implemented.
package config

import "sort"

// Config is the top-level local-mode document.
type Config struct {
	Version int         `yaml:"version"`
	Mode    string      `yaml:"mode"`
	Policy  PolicySpec  `yaml:"policy"`
	Disks   DisksSpec   `yaml:"disks"`
	Runtime RuntimeSpec `yaml:"runtime"`
}

// modePeek decodes just enough of the document to route it: a local-mode
// version-1 document is parsed strictly against Config; anything else is
// rejected before a field-by-field decode is attempted.
type modePeek struct {
	// any, not int: yaml.v3 truncates 1.9 to 1 when decoding into an int.
	Version any    `yaml:"version"`
	Mode    string `yaml:"mode"`
}

// PolicySpec is a partial policy override. Every field is a pointer so the
// resolver can tell "not set, inherit" apart from a zero value.
type PolicySpec struct {
	Limits   *LimitsSpec   `yaml:"limits"`
	Advanced *AdvancedSpec `yaml:"advanced"`
}

type LimitsSpec struct {
	MinIOPS *int `yaml:"min_iops"`
	MaxIOPS *int `yaml:"max_iops"`
}

// AdvancedSpec holds experimental controller parameters.
// billing_step_iops is deliberately absent: it is platform-fixed, so strict
// decoding rejects it as an unknown key.
type AdvancedSpec struct {
	Evaluation *EvaluationSpec `yaml:"evaluation"`
	Saturation *SaturationSpec `yaml:"saturation"`
	Target     *TargetSpec     `yaml:"target"`
	Cooldown   *CooldownSpec   `yaml:"cooldown"`
	Step       *StepSpec       `yaml:"step"`
	Budget     *BudgetSpec     `yaml:"budget"`
}

// EvaluationSpec has no collection_interval of its own — sampling
// cadence is process-wide (runtime.metrics.collection_interval), since one
// Sampler ticks one source for every managed disk in a single call; a
// per-policy override could never actually be honored. Window readiness is
// computed from each Sample's own Timestamp, not a count derived from an
// assumed interval, so nothing here depends on knowing the cadence anyway.
type EvaluationSpec struct {
	ScaleUpWindow   *Duration `yaml:"scale_up_window"`
	ScaleDownWindow *Duration `yaml:"scale_down_window"`
}

type SaturationSpec struct {
	// UtilizationThreshold gates on whichever of IOPS or bandwidth
	// utilization is higher, not IOPS alone.
	UtilizationThreshold *float64     `yaml:"utilization_threshold"`
	QueueDepthThreshold  *float64     `yaml:"queue_depth_threshold"`
	Latency              *LatencySpec `yaml:"latency"`
}

type LatencySpec struct {
	// Mode: only "relative" is accepted (Validate rejects anything else,
	// including "absolute"). Absolute latency thresholds are out of scope,
	// and no Analyzer code path reads this field.
	Mode               *string  `yaml:"mode"`
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

// DisksSpec is the disk selection strategy.
// Policies is keyed by device_name — the exact value VM status reports,
// never a kernel device path. In selected mode its keys
// are the managed set; in auto-discovery mode they override single disks.
type DisksSpec struct {
	Mode     string                 `yaml:"mode"` // auto-discovery | selected
	Exclude  []string               `yaml:"exclude"`
	Policies map[string]*PolicySpec `yaml:"policies"`
}

type RuntimeSpec struct {
	Metrics    MetricsSpec    `yaml:"metrics"`
	Controller ControllerSpec `yaml:"controller"`
}

// MetricsSpec.CollectionInterval is the single canonical sampling cadence —
// there is no policy-level equivalent to conflict with.
type MetricsSpec struct {
	Source             string    `yaml:"source"` // guest | platform
	CollectionInterval *Duration `yaml:"collection_interval"`
}

type ControllerSpec struct {
	ReconcileInterval *Duration `yaml:"reconcile_interval"`
	DiscoveryInterval *Duration `yaml:"discovery_interval"`
}

// PolicyNames returns the keys of Policies in sorted order, so selection and
// diagnostics do not depend on map iteration order.
func (d DisksSpec) PolicyNames() []string {
	names := make([]string, 0, len(d.Policies))
	for name := range d.Policies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
