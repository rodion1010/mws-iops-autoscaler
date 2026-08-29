package config

import (
	"testing"
	"time"
)

func TestResolve_diskInheritsVMPolicyWhenNoOverride(t *testing.T) {
	cfg := validConfig()
	cfg.Disks.Mode = "selected"
	cfg.Disks.Exclude = nil
	cfg.Disks.Selected = []SelectedDiskSpec{{DeviceName: "datadisk"}}

	resolved := Resolve(cfg)
	got := resolved.DiskPolicies["datadisk"]
	if got != resolved.VMPolicy {
		t.Fatalf("disk with no override = %+v, want the VM policy %+v", got, resolved.VMPolicy)
	}
}

func TestResolve_diskOverridesOnlyLimitsInheritsAdvanced(t *testing.T) {
	cfg := validConfig() // vm-level: [1000, 10000], the one built-in advanced set
	cfg.Disks.Mode = "selected"
	cfg.Disks.Exclude = nil
	cfg.Disks.Selected = []SelectedDiskSpec{{
		DeviceName: "logdisk",
		Policy:     &PolicySpec{Limits: &LimitsSpec{MinIOPS: intPtr(3000)}},
	}}

	resolved := Resolve(cfg)
	got := resolved.DiskPolicies["logdisk"]

	if got.MinIOPS != 3000 {
		t.Errorf("MinIOPS = %d, want overridden 3000", got.MinIOPS)
	}
	// max_iops was not overridden — inherited from the VM level.
	if got.MaxIOPS != 10000 {
		t.Errorf("MaxIOPS = %d, want inherited 10000", got.MaxIOPS)
	}
	// Advanced was not touched at all — inherited unchanged.
	if got.Advanced != resolved.VMPolicy.Advanced {
		t.Errorf("Advanced = %+v, want inherited from the VM policy %+v", got.Advanced, resolved.VMPolicy.Advanced)
	}
}

func TestResolve_diskOverridesLimitsAndAdvanced(t *testing.T) {
	cfg := validConfig()
	cfg.Disks.Mode = "selected"
	cfg.Disks.Exclude = nil
	cfg.Disks.Selected = []SelectedDiskSpec{{
		DeviceName: "backupdisk",
		Policy: &PolicySpec{
			Limits:   &LimitsSpec{MaxIOPS: intPtr(6000)},
			Advanced: &AdvancedSpec{Enabled: boolPtr(true), Step: &StepSpec{MinChangeIOPS: intPtr(300)}},
		},
	}}

	resolved := Resolve(cfg)
	got := resolved.DiskPolicies["backupdisk"]

	if got.MinIOPS != 1000 {
		t.Errorf("MinIOPS = %d, want inherited 1000", got.MinIOPS)
	}
	if got.MaxIOPS != 6000 {
		t.Errorf("MaxIOPS = %d, want overridden 6000", got.MaxIOPS)
	}
	if got.Advanced.MinChangeIOPS != 300 {
		t.Errorf("MinChangeIOPS = %d, want overridden 300", got.Advanced.MinChangeIOPS)
	}
	// Fields the override did not touch still come from the default set.
	if got.Advanced.ScaleUpWindow != defaultAdvanced.ScaleUpWindow {
		t.Errorf("ScaleUpWindow changed even though it was not overridden")
	}
}

func TestResolve_advancedOverrideAppliesOnTopOfDefault(t *testing.T) {
	cfg := validConfig()
	cfg.Policy.Advanced = &AdvancedSpec{
		Enabled: boolPtr(true),
		Step:    &StepSpec{MinChangeIOPS: intPtr(999)},
	}

	resolved := Resolve(cfg)
	if resolved.VMPolicy.Advanced.MinChangeIOPS != 999 {
		t.Errorf("MinChangeIOPS = %d, want 999", resolved.VMPolicy.Advanced.MinChangeIOPS)
	}
	// Everything else in Advanced still comes from the built-in default.
	if resolved.VMPolicy.Advanced.ScaleUpWindow != defaultAdvanced.ScaleUpWindow {
		t.Errorf("ScaleUpWindow changed even though it was not overridden")
	}
}

func TestResolve_omittedPolicyUsesPlatformDefault(t *testing.T) {
	cfg := &Config{
		Mode: "local",
		Disks: DisksSpec{
			Mode: "auto-discovery",
		},
		Runtime: RuntimeSpec{Metrics: MetricsSpec{Source: "guest"}},
	}
	resolved := Resolve(cfg)
	if resolved.VMPolicy.MinIOPS != DefaultMinIOPS || resolved.VMPolicy.MaxIOPS != DefaultMaxIOPS {
		t.Errorf("limits = [%d,%d], want defaults [%d,%d]",
			resolved.VMPolicy.MinIOPS, resolved.VMPolicy.MaxIOPS, DefaultMinIOPS, DefaultMaxIOPS)
	}
	if resolved.VMPolicy.Advanced != defaultAdvanced {
		t.Errorf("Advanced = %+v, want the built-in default %+v", resolved.VMPolicy.Advanced, defaultAdvanced)
	}
}

func TestResolve_runtimeDefaults(t *testing.T) {
	cfg := validConfig()
	resolved := Resolve(cfg)
	if resolved.Runtime.ReconcileInterval != defaultReconcileInterval {
		t.Errorf("ReconcileInterval = %v, want default %v", resolved.Runtime.ReconcileInterval, defaultReconcileInterval)
	}
	if resolved.Runtime.DiscoveryInterval != defaultDiscoveryInterval {
		t.Errorf("DiscoveryInterval = %v, want default %v", resolved.Runtime.DiscoveryInterval, defaultDiscoveryInterval)
	}
}

func TestSamplerCapacity(t *testing.T) {
	// issue #6, reopened: this is the exact formula both the orchestrator
	// (sizing the real Sampler) and Validate (rejecting a config that
	// would size it too small) share — one implementation, tested once.
	cases := []struct {
		name                string
		vmScaleDownWindow   time.Duration
		diskScaleDownWindow time.Duration // 0 means no per-disk override
		collectionInterval  time.Duration
		want                int
	}{
		{"default preset", 30 * time.Minute, 0, 10 * time.Second, 180},
		{"exact boundary yields exactly 3", 30 * time.Minute, 0, 10 * time.Minute, 3},
		{"floors down, not up", 30 * time.Minute, 0, 11 * time.Minute, 2},
		{"per-disk override longer than VM policy wins", 5 * time.Minute, 40 * time.Minute, 10 * time.Minute, 4},
		{"per-disk override shorter than VM policy does not win", 30 * time.Minute, 5 * time.Minute, 10 * time.Minute, 3},
		{"never floors below 1", time.Minute, 0, time.Hour, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := &ResolvedConfig{
				VMPolicy: ResolvedPolicy{Advanced: ResolvedAdvanced{ScaleDownWindow: tc.vmScaleDownWindow}},
				Runtime:  ResolvedRuntime{CollectionInterval: tc.collectionInterval},
			}
			if tc.diskScaleDownWindow > 0 {
				rc.DiskPolicies = map[string]ResolvedPolicy{
					"datadisk": {Advanced: ResolvedAdvanced{ScaleDownWindow: tc.diskScaleDownWindow}},
				}
			}
			if got := rc.SamplerCapacity(); got != tc.want {
				t.Errorf("SamplerCapacity() = %d, want %d", got, tc.want)
			}
		})
	}
}
