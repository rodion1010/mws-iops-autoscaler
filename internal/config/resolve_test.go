package config

import (
	"testing"
	"time"
)

func TestResolve_diskInheritsVMPolicyWhenNoOverride(t *testing.T) {
	cfg := validConfig()
	cfg.Disks.Mode = "selected"
	cfg.Disks.Exclude = nil
	cfg.Disks.Policies = map[string]*PolicySpec{"datadisk": nil}

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
	cfg.Disks.Policies = map[string]*PolicySpec{
		"logdisk": &PolicySpec{Limits: &LimitsSpec{MinIOPS: intPtr(3000)}},
	}

	resolved := Resolve(cfg)
	got := resolved.DiskPolicies["logdisk"]

	if got.MinIOPS != 3000 {
		t.Errorf("MinIOPS = %d, want overridden 3000", got.MinIOPS)
	}
	if got.MaxIOPS != 10000 {
		t.Errorf("MaxIOPS = %d, want inherited 10000", got.MaxIOPS)
	}
	if got.Advanced != resolved.VMPolicy.Advanced {
		t.Errorf("Advanced = %+v, want inherited from the VM policy %+v", got.Advanced, resolved.VMPolicy.Advanced)
	}
}

func TestResolve_diskOverridesLimitsAndAdvanced(t *testing.T) {
	cfg := validConfig()
	cfg.Disks.Mode = "selected"
	cfg.Disks.Exclude = nil
	cfg.Disks.Policies = map[string]*PolicySpec{
		"backupdisk": &PolicySpec{
			Limits:   &LimitsSpec{MaxIOPS: intPtr(6000)},
			Advanced: &AdvancedSpec{Step: &StepSpec{MinChangeIOPS: intPtr(300)}},
		},
	}

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
	if got.Advanced.ScaleUpWindow != defaultAdvanced.ScaleUpWindow {
		t.Errorf("ScaleUpWindow changed even though it was not overridden")
	}
}

// auto-discovery keeps managing every disk while one of them carries its own policy.
func TestResolve_autoDiscoveryAcceptsPerDiskPolicy(t *testing.T) {
	cfg := validConfig() // auto-discovery, excludes mws-bootdisk
	cfg.Disks.Policies = map[string]*PolicySpec{
		"mws-wal": {Limits: &LimitsSpec{MinIOPS: intPtr(5000)}},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if got := Resolve(cfg).DiskPolicies["mws-wal"].MinIOPS; got != 5000 {
		t.Errorf("mws-wal MinIOPS = %d, want 5000", got)
	}
}

func TestResolve_advancedOverrideAppliesOnTopOfDefault(t *testing.T) {
	cfg := validConfig()
	cfg.Policy.Advanced = &AdvancedSpec{
		Step: &StepSpec{MinChangeIOPS: intPtr(999)},
	}

	resolved := Resolve(cfg)
	if resolved.VMPolicy.Advanced.MinChangeIOPS != 999 {
		t.Errorf("MinChangeIOPS = %d, want 999", resolved.VMPolicy.Advanced.MinChangeIOPS)
	}
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
	// Capacity is ceil(window/interval)+1 endpoint sample+2 slack samples.
	cases := []struct {
		name                string
		vmScaleDownWindow   time.Duration
		diskScaleDownWindow time.Duration // 0 means no per-disk override
		collectionInterval  time.Duration
		want                int
	}{
		// ceil(30m/10s)+3 = 183.
		{"default preset", 30 * time.Minute, 0, 10 * time.Second, 183},
		// ceil(30m/10m)+3 = 6.
		{"evenly divisible window/interval", 30 * time.Minute, 0, 10 * time.Minute, 6},
		// ceil(30m/11m)+3 = 6, proving ceiling rather than floor.
		{"non-divisible window/interval rounds up, not down", 30 * time.Minute, 0, 11 * time.Minute, 6},
		// ceil(30m/13m)+3 = 6.
		{"just over two intervals still rounds up to three", 30 * time.Minute, 0, 13 * time.Minute, 6},
		{"per-disk override longer than VM policy wins", 5 * time.Minute, 40 * time.Minute, 10 * time.Minute, 7},
		{"per-disk override shorter than VM policy does not win", 30 * time.Minute, 5 * time.Minute, 10 * time.Minute, 6},
		// Even an extreme ratio retains the four-sample floor.
		{"minimum floor is 1 + endpoint + slack, never lower", time.Minute, 0, time.Hour, 4},
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

func TestSamplerCapacity_spansTheFullWindowAtExactCadence(t *testing.T) {
	// The returned capacity must span at least the configured endpoint-to-endpoint window.
	window := 30 * time.Minute
	interval := time.Minute
	rc := &ResolvedConfig{
		VMPolicy: ResolvedPolicy{Advanced: ResolvedAdvanced{ScaleDownWindow: window}},
		Runtime:  ResolvedRuntime{CollectionInterval: interval},
	}
	n := rc.SamplerCapacity()
	span := time.Duration(n-1) * interval
	if span < window {
		t.Errorf("a capacity-%d buffer at a %s cadence spans only %s end to end, want >= the %s window",
			n, interval, span, window)
	}
}
