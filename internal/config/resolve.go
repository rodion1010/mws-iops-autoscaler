package config

import "time"

// ResolvedPolicy has no remaining inherited values.
type ResolvedPolicy struct {
	MinIOPS  int
	MaxIOPS  int
	Advanced ResolvedAdvanced
}

// ResolvedRuntime has defaults filled and one process-wide collection cadence.
type ResolvedRuntime struct {
	MetricsSource      string
	CollectionInterval time.Duration
	ReconcileInterval  time.Duration
	DiscoveryInterval  time.Duration
}

// ResolvedConfig is the fully inherited local-mode configuration.
type ResolvedConfig struct {
	VMPolicy     ResolvedPolicy
	Disks        DisksSpec
	DiskPolicies map[string]ResolvedPolicy // device_name -> resolved policy, "selected" mode only
	Runtime      ResolvedRuntime
}

// defaultResolvedPolicy is the root of policy inheritance.
func defaultResolvedPolicy() ResolvedPolicy {
	return ResolvedPolicy{
		MinIOPS:  DefaultMinIOPS,
		MaxIOPS:  DefaultMaxIOPS,
		Advanced: defaultAdvanced,
	}
}

// Resolve applies policy inheritance and runtime defaults to a validated config.
func Resolve(cfg *Config) *ResolvedConfig {
	vmPolicy := resolvePolicy(defaultResolvedPolicy(), &cfg.Policy)

	diskPolicies := make(map[string]ResolvedPolicy)
	for _, d := range cfg.Disks.Selected {
		diskPolicies[d.DeviceName] = resolvePolicy(vmPolicy, d.Policy)
	}

	return &ResolvedConfig{
		VMPolicy:     vmPolicy,
		Disks:        cfg.Disks,
		DiskPolicies: diskPolicies,
		Runtime:      resolveRuntime(cfg.Runtime),
	}
}

// resolvePolicy overlays explicitly set child fields on an already resolved parent.
func resolvePolicy(parent ResolvedPolicy, child *PolicySpec) ResolvedPolicy {
	result := parent

	if child != nil && child.Limits != nil {
		if child.Limits.MinIOPS != nil {
			result.MinIOPS = *child.Limits.MinIOPS
		}
		if child.Limits.MaxIOPS != nil {
			result.MaxIOPS = *child.Limits.MaxIOPS
		}
	}

	if child != nil && child.Advanced != nil {
		applyAdvancedOverride(&result.Advanced, child.Advanced)
	}

	return result
}

func applyAdvancedOverride(dst *ResolvedAdvanced, src *AdvancedSpec) {
	if e := src.Evaluation; e != nil {
		if e.ScaleUpWindow != nil {
			dst.ScaleUpWindow = time.Duration(*e.ScaleUpWindow)
		}
		if e.ScaleDownWindow != nil {
			dst.ScaleDownWindow = time.Duration(*e.ScaleDownWindow)
		}
	}
	if s := src.Saturation; s != nil {
		if s.UtilizationThreshold != nil {
			dst.UtilizationThreshold = *s.UtilizationThreshold
		}
		if s.QueueDepthThreshold != nil {
			dst.QueueDepthThreshold = *s.QueueDepthThreshold
		}
		if l := s.Latency; l != nil {
			if l.Mode != nil {
				dst.LatencyMode = *l.Mode
			}
			if l.BaselineMultiplier != nil {
				dst.LatencyBaselineMultiplier = *l.BaselineMultiplier
			}
		}
	}
	if t := src.Target; t != nil {
		if t.ScaleUpUtilization != nil {
			dst.ScaleUpUtilization = *t.ScaleUpUtilization
		}
		if t.ScaleDownUtilization != nil {
			dst.ScaleDownUtilization = *t.ScaleDownUtilization
		}
	}
	if c := src.Cooldown; c != nil {
		if c.ScaleUp != nil {
			dst.CooldownScaleUp = time.Duration(*c.ScaleUp)
		}
		if c.ScaleDown != nil {
			dst.CooldownScaleDown = time.Duration(*c.ScaleDown)
		}
	}
	if st := src.Step; st != nil {
		if st.MinChangeIOPS != nil {
			dst.MinChangeIOPS = *st.MinChangeIOPS
		}
		if st.MaxScaleUpRatio != nil {
			dst.MaxScaleUpRatio = *st.MaxScaleUpRatio
		}
		if st.MaxScaleDownRatio != nil {
			dst.MaxScaleDownRatio = *st.MaxScaleDownRatio
		}
	}
	if b := src.Budget; b != nil && b.MaxChangesPer24h != nil {
		dst.MaxChangesPer24h = *b.MaxChangesPer24h
	}
}

// Runtime defaults.
const (
	defaultMetricsSource      = "guest"
	defaultCollectionInterval = 10 * time.Second
	defaultReconcileInterval  = 30 * time.Second
	defaultDiscoveryInterval  = 5 * time.Minute
)

// samplerCapacitySlackSamples tolerates jitter and occasional missing samples beyond the exact span.
const samplerCapacitySlackSamples = 2

// SamplerCapacity sizes the shared ring for the longest resolved policy window.
// Endpoint span requires ceil(window/interval)+1 samples; additional slack tolerates jitter.
// Exporting the formula keeps validation and sampler construction consistent.
func (rc *ResolvedConfig) SamplerCapacity() int {
	longest := rc.VMPolicy.Advanced.ScaleDownWindow
	for _, p := range rc.DiskPolicies {
		if p.Advanced.ScaleDownWindow > longest {
			longest = p.Advanced.ScaleDownWindow
		}
	}
	if rc.Runtime.CollectionInterval <= 0 || longest <= 0 {
		// Keep this method safe if called with an unvalidated non-positive duration.
		return 1
	}
	n := int((longest+rc.Runtime.CollectionInterval-1)/rc.Runtime.CollectionInterval) + 1 + samplerCapacitySlackSamples
	if n < 1 {
		n = 1
	}
	return n
}

func resolveRuntime(r RuntimeSpec) ResolvedRuntime {
	out := ResolvedRuntime{
		MetricsSource:      defaultMetricsSource,
		CollectionInterval: defaultCollectionInterval,
		ReconcileInterval:  defaultReconcileInterval,
		DiscoveryInterval:  defaultDiscoveryInterval,
	}
	if r.Metrics.Source != "" {
		out.MetricsSource = r.Metrics.Source
	}
	if r.Metrics.CollectionInterval != nil {
		out.CollectionInterval = time.Duration(*r.Metrics.CollectionInterval)
	}
	if r.Controller.ReconcileInterval != nil {
		out.ReconcileInterval = time.Duration(*r.Controller.ReconcileInterval)
	}
	if r.Controller.DiscoveryInterval != nil {
		out.DiscoveryInterval = time.Duration(*r.Controller.DiscoveryInterval)
	}
	return out
}
