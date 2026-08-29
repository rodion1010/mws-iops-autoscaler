package config

import "time"

// ResolvedPolicy is a policy with nothing left to inherit.
type ResolvedPolicy struct {
	MinIOPS  int
	MaxIOPS  int
	Advanced ResolvedAdvanced
}

// ResolvedRuntime is the runtime block with defaults filled in.
// CollectionInterval here is the single canonical sampling cadence (ADR-010)
// — one Sampler ticks one source for every managed disk in a single call,
// so there is no per-policy equivalent to keep in sync with this one.
type ResolvedRuntime struct {
	MetricsSource      string
	CollectionInterval time.Duration
	ReconcileInterval  time.Duration
	DiscoveryInterval  time.Duration
}

// ResolvedConfig is the fully resolved local-mode config. Discovery (M1) maps
// live disks against DiskPolicies / Disks to build the managed set; this
// package only resolves policy, since it has no access to the live disk list.
type ResolvedConfig struct {
	VMPolicy     ResolvedPolicy
	Disks        DisksSpec
	DiskPolicies map[string]ResolvedPolicy // device_name -> resolved policy, "selected" mode only
	Runtime      ResolvedRuntime
}

// defaultResolvedPolicy is the platform-default rung of the inheritance chain
// (docs/configuration.md §5): default limits and the one built-in advanced set.
func defaultResolvedPolicy() ResolvedPolicy {
	return ResolvedPolicy{
		MinIOPS:  DefaultMinIOPS,
		MaxIOPS:  DefaultMaxIOPS,
		Advanced: defaultAdvanced,
	}
}

// Resolve merges the config's policy inheritance chain and fills in runtime
// defaults. It assumes cfg has already passed Validate — in particular, that
// no advanced field is set without advanced.enabled: true.
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

// resolvePolicy merges parent (already resolved) with child (a partial
// override, possibly nil). "An object inherits the whole policy, then
// overrides only the fields it sets explicitly" (docs/configuration.md §5).
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

// Defaults for the runtime block (docs/configuration.md §6 examples).
const (
	defaultMetricsSource      = "guest"
	defaultCollectionInterval = 10 * time.Second
	defaultReconcileInterval  = 30 * time.Second
	defaultDiscoveryInterval  = 5 * time.Minute
)

// SamplerCapacity sizes the ring buffer by the longest configured window
// across the VM policy and every per-disk override — Sampler.NewSampler
// (design.md §3.2 "Bounded memory") takes one capacity for every disk, so
// it must fit whichever disk needs the most history. Exported so both the
// orchestrator (which sizes the real Sampler, cmd/mws-iops-autoscaler)
// and Validate (which rejects a config that would size it too small to
// ever complete a window, issue #6) share exactly one formula — never two
// independently-reasoned-about copies that could silently drift apart.
func (rc *ResolvedConfig) SamplerCapacity() int {
	longest := rc.VMPolicy.Advanced.ScaleDownWindow
	for _, p := range rc.DiskPolicies {
		if p.Advanced.ScaleDownWindow > longest {
			longest = p.Advanced.ScaleDownWindow
		}
	}
	if rc.Runtime.CollectionInterval <= 0 {
		// Defensive only — validateResolvedRuntime already rejects a
		// non-positive collection_interval outright; this just keeps the
		// method itself panic-free (no division by zero) if ever called
		// against an unvalidated ResolvedConfig.
		return 1
	}
	n := int(longest / rc.Runtime.CollectionInterval)
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
