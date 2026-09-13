package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		APIVersion: "autoscaling.mws.cloud/v1alpha1",
		Kind:       "IOPSAutoScaler",
		Mode:       "local",
		Policy: PolicySpec{
			Limits: &LimitsSpec{MinIOPS: intPtr(1000), MaxIOPS: intPtr(10000)},
		},
		Disks: DisksSpec{
			Mode:    "auto-discovery",
			Exclude: []string{"mws-bootdisk"},
		},
		Runtime: RuntimeSpec{
			Metrics: MetricsSpec{Source: "guest"},
		},
	}
}

func TestValidate_valid(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("Validate(valid config) = %v, want nil", err)
	}
}

// TestValidateDeviceNameAcceptsPlatformVisibleName confirms a device_name
// written exactly as VM status reports it must validate, not be rejected
// as if it were a config mistake.
func TestValidateDeviceNameAcceptsPlatformVisibleName(t *testing.T) {
	c := validConfig()
	c.Disks.Mode = "selected"
	c.Disks.Exclude = nil
	c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "mws-datadisk"}}

	if err := Validate(c); err != nil {
		t.Fatalf("Validate(platform-visible device_name) = %v, want nil", err)
	}
}

// TestValidateDeviceNameRejectsNonPlatformVisibleForm confirms a
// device_name that cannot be the exact value VM status reports is caught
// at --validate-config time, with an actionable diagnostic, in both
// selected and exclude — rather than passing validation and only failing
// later during discovery with a generic "not found".
func TestValidateDeviceNameRejectsNonPlatformVisibleForm(t *testing.T) {
	t.Run("selected", func(t *testing.T) {
		c := validConfig()
		c.Disks.Mode = "selected"
		c.Disks.Exclude = nil
		c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "datadisk"}}

		err := Validate(c)
		if err == nil || !strings.Contains(err.Error(), "is not the exact value VM status reports") {
			t.Fatalf("Validate() = %v, want an error naming the missing mws- prefix", err)
		}
	})

	t.Run("exclude", func(t *testing.T) {
		c := validConfig()
		c.Disks.Exclude = []string{"datadisk"}

		err := Validate(c)
		if err == nil || !strings.Contains(err.Error(), "is not the exact value VM status reports") {
			t.Fatalf("Validate() = %v, want an error naming the missing mws- prefix", err)
		}
	})
}

func TestValidate_rules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string // substring expected in the error message
	}{
		{
			name:    "min_iops below platform minimum",
			mutate:  func(c *Config) { c.Policy.Limits.MinIOPS = intPtr(500) },
			wantSub: "below the platform minimum",
		},
		{
			name:    "max_iops above platform maximum",
			mutate:  func(c *Config) { c.Policy.Limits.MaxIOPS = intPtr(20000) },
			wantSub: "above the platform maximum",
		},
		{
			name: "min_iops greater than max_iops",
			mutate: func(c *Config) {
				c.Policy.Limits.MinIOPS = intPtr(9000)
				c.Policy.Limits.MaxIOPS = intPtr(2000)
			},
			wantSub: "greater than max_iops",
		},
		{
			name:    "min_iops not a multiple of 100",
			mutate:  func(c *Config) { c.Policy.Limits.MinIOPS = intPtr(1050) },
			wantSub: "not a multiple of",
		},
		{
			name:    "max_iops not a multiple of 100",
			mutate:  func(c *Config) { c.Policy.Limits.MaxIOPS = intPtr(9990) },
			wantSub: "not a multiple of",
		},
		{
			name: "advanced fields set without advanced.enabled",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Step: &StepSpec{MinChangeIOPS: intPtr(500)},
				}
			},
			wantSub: "advanced.enabled is not true",
		},
		{
			name: "advanced fields set with advanced.enabled: false",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(false),
					Step:    &StepSpec{MinChangeIOPS: intPtr(500)},
				}
			},
			wantSub: "advanced.enabled is not true",
		},
		{
			name: "advanced.enabled: true with no fields set is fine",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{Enabled: boolPtr(true)}
			},
			wantSub: "",
		},
		{
			name: "device_name as a kernel device path",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Exclude = nil
				c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "/dev/vdb"}}
			},
			wantSub: "kernel device path",
		},
		{
			name: "device_name matching the exact value VM status reports is accepted",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Exclude = nil
				c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "mws-datadisk"}}
			},
			wantSub: "",
		},
		{
			name: "duplicate device_name within selected",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Exclude = nil
				c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "mws-datadisk"}, {DeviceName: "mws-datadisk"}}
			},
			wantSub: "listed more than once",
		},
		{
			name: "duplicate device_name within exclude",
			mutate: func(c *Config) {
				c.Disks.Exclude = []string{"mws-bootdisk", "mws-bootdisk"}
			},
			wantSub: "listed more than once",
		},
		{
			name: "platform source in local mode without an API key configured",
			mutate: func(c *Config) {
				c.Runtime.Metrics.Source = "platform"
			},
			wantSub: "requires a monitoring API key",
		},
		{
			name: "scale_down_window shorter than scale_up_window",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Evaluation: &EvaluationSpec{
						ScaleUpWindow:   durationPtr("10m"),
						ScaleDownWindow: durationPtr("5m"),
					},
				}
			},
			wantSub: "invites flapping",
		},
		{
			name: "cooldown.scale_up longer than scale_up_window",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Evaluation: &EvaluationSpec{
						ScaleUpWindow: durationPtr("5m"),
					},
					Cooldown: &CooldownSpec{
						ScaleUp: durationPtr("10m"),
					},
				}
			},
			wantSub: "the agent could never act",
		},
		{
			name: "cooldown.scale_down longer than scale_down_window",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Evaluation: &EvaluationSpec{
						ScaleDownWindow: durationPtr("30m"),
					},
					Cooldown: &CooldownSpec{
						ScaleDown: durationPtr("1h"),
					},
				}
			},
			wantSub: "cooldown.scale_down",
		},
		{
			name:    "unsupported mode",
			mutate:  func(c *Config) { c.Mode = "fleet" },
			wantSub: "not supported in this build",
		},
		{
			name:    "bogus disks.mode",
			mutate:  func(c *Config) { c.Disks.Mode = "all-of-them" },
			wantSub: "neither \"auto-discovery\" nor \"selected\"",
		},
		{
			name:    "empty disks.mode",
			mutate:  func(c *Config) { c.Disks.Mode = "" },
			wantSub: "neither \"auto-discovery\" nor \"selected\"",
		},
		{
			name: "mode: auto-discovery with selected populated",
			mutate: func(c *Config) {
				c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "datadisk"}}
			},
			wantSub: "selected only applies to mode: selected",
		},
		{
			name: "mode: selected with exclude set",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "datadisk"}}
				// validConfig already excludes bootdisk.
			},
			wantSub: "exclude only applies to mode: auto-discovery",
		},
		{
			name: "mode: selected with an empty selected list",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Exclude = nil
			},
			wantSub: "mode is \"selected\" but the list is empty",
		},
		{
			name: "negative scale_up_window",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Evaluation: &EvaluationSpec{ScaleUpWindow: negativeDurationPtr(t, "5m")},
				}
			},
			wantSub: "evaluation.scale_up_window",
		},
		{
			name: "negative cooldown.scale_down",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:  boolPtr(true),
					Cooldown: &CooldownSpec{ScaleDown: negativeDurationPtr(t, "5m")},
				}
			},
			wantSub: "cooldown.scale_down",
		},
		{
			name: "utilization_threshold of zero",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{UtilizationThreshold: floatPtr(0)},
				}
			},
			wantSub: "saturation.utilization_threshold",
		},
		{
			name: "utilization_threshold above 1",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{UtilizationThreshold: floatPtr(1.5)},
				}
			},
			wantSub: "saturation.utilization_threshold",
		},
		{
			name: "scale_up_utilization of zero",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Target:  &TargetSpec{ScaleUpUtilization: floatPtr(0)},
				}
			},
			wantSub: "target.scale_up_utilization",
		},
		{
			name: "negative queue_depth_threshold",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{QueueDepthThreshold: floatPtr(-1)},
				}
			},
			wantSub: "saturation.queue_depth_threshold",
		},
		{
			name: "unknown latency.mode",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{Latency: &LatencySpec{Mode: strPtr("eventual")}},
				}
			},
			wantSub: "latency.mode",
		},
		{
			name: "zero latency.baseline_multiplier",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{Latency: &LatencySpec{BaselineMultiplier: floatPtr(0)}},
				}
			},
			wantSub: "latency.baseline_multiplier",
		},
		{
			name: "min_change_iops not a multiple of 100",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Step:    &StepSpec{MinChangeIOPS: intPtr(150)},
				}
			},
			wantSub: "step.min_change_iops",
		},
		{
			name: "zero min_change_iops",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Step:    &StepSpec{MinChangeIOPS: intPtr(0)},
				}
			},
			wantSub: "step.min_change_iops",
		},
		{
			name: "max_scale_up_ratio at or below 1",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Step:    &StepSpec{MaxScaleUpRatio: floatPtr(1)},
				}
			},
			wantSub: "step.max_scale_up_ratio",
		},
		{
			name: "max_scale_down_ratio at or above 1",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Step:    &StepSpec{MaxScaleDownRatio: floatPtr(1)},
				}
			},
			wantSub: "step.max_scale_down_ratio",
		},
		{
			name: "zero max_changes_per_24h",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Budget:  &BudgetSpec{MaxChangesPer24h: intPtr(0)},
				}
			},
			wantSub: "budget.max_changes_per_24h",
		},
		{
			name: "zero runtime collection_interval",
			mutate: func(c *Config) {
				c.Runtime.Metrics.CollectionInterval = durationPtr("0s")
			},
			wantSub: "runtime.metrics.collection_interval",
		},
		{
			name: "latency.mode absolute is rejected (not implemented)",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{Latency: &LatencySpec{Mode: strPtr("absolute")}},
				}
			},
			wantSub: "latency.mode",
		},
		{
			name: "NaN utilization_threshold rejected as non-finite, not silently passed",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{UtilizationThreshold: floatPtr(math.NaN())},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "+Inf scale_up_utilization rejected as non-finite",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Target:  &TargetSpec{ScaleUpUtilization: floatPtr(math.Inf(1))},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "NaN queue_depth_threshold rejected as non-finite",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{QueueDepthThreshold: floatPtr(math.NaN())},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "queue_depth_threshold of exactly zero rejected — gate must not degenerate to always-true",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{QueueDepthThreshold: floatPtr(0)},
				}
			},
			wantSub: "saturation.queue_depth_threshold",
		},
		{
			name: "queue_depth_threshold just above zero is valid",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{QueueDepthThreshold: floatPtr(0.1)},
				}
			},
			wantSub: "",
		},
		{
			name: "latency.baseline_multiplier of exactly 1 rejected — threshold would equal the baseline itself",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{Latency: &LatencySpec{BaselineMultiplier: floatPtr(1)}},
				}
			},
			wantSub: "latency.baseline_multiplier",
		},
		{
			name: "latency.baseline_multiplier just above 1 is valid",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled:    boolPtr(true),
					Saturation: &SaturationSpec{Latency: &LatencySpec{BaselineMultiplier: floatPtr(1.01)}},
				}
			},
			wantSub: "",
		},
		{
			name: "NaN max_scale_up_ratio rejected as non-finite",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Step:    &StepSpec{MaxScaleUpRatio: floatPtr(math.NaN())},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "Inf max_scale_down_ratio rejected as non-finite",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Enabled: boolPtr(true),
					Step:    &StepSpec{MaxScaleDownRatio: floatPtr(math.Inf(-1))},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "collection_interval longer than scale_up_window is rejected even though it fits scale_down_window",
			mutate: func(c *Config) {
				// A 20-minute cadence cannot populate the default five-minute scale-up window.
				c.Runtime.Metrics.CollectionInterval = durationPtr("20m")
			},
			wantSub: "scale_up_window",
		},
		{
			name: "collection_interval within every configured window is valid",
			mutate: func(c *Config) {
				c.Runtime.Metrics.CollectionInterval = durationPtr("1m") // <= both 5m and 30m defaults
			},
			wantSub: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Keep the platform-key case independent of the ambient environment.
			t.Setenv(monitoringAPIKeyPathEnv, "")

			cfg := validConfig()
			tc.mutate(cfg)

			err := Validate(cfg)

			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidate_perDiskAdvancedGate(t *testing.T) {
	cfg := validConfig()
	cfg.Disks.Mode = "selected"
	cfg.Disks.Exclude = nil
	cfg.Disks.Selected = []SelectedDiskSpec{
		{
			DeviceName: "mws-datadisk",
			Policy: &PolicySpec{
				Advanced: &AdvancedSpec{Step: &StepSpec{MinChangeIOPS: intPtr(500)}},
			},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "advanced.enabled is not true") {
		t.Fatalf("Validate() = %v, want a per-disk advanced.enabled violation", err)
	}
}

func intPtr(i int) *int           { return &i }
func boolPtr(b bool) *bool        { return &b }
func floatPtr(f float64) *float64 { return &f }
func strPtr(s string) *string     { return &s }

func durationPtr(s string) *Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	dd := Duration(d)
	return &dd
}

func negativeDurationPtr(t *testing.T, s string) *Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("parse duration %q: %v", s, err)
	}
	dd := Duration(-d)
	return &dd
}

func TestValidateSamplerCapacity_directDefenseInDepth(t *testing.T) {
	// Exercise the capacity-floor backstop independently of the current sizing formula.
	rc := &ResolvedConfig{
		VMPolicy: ResolvedPolicy{Advanced: ResolvedAdvanced{ScaleDownWindow: 5 * time.Minute}},
		Runtime:  ResolvedRuntime{CollectionInterval: time.Minute},
	}
	if got := rc.SamplerCapacity(); got < minSamplesForWindowMirror {
		t.Fatalf("test setup: SamplerCapacity() = %d, want >= %d so this test isn't accidentally exercising the real bug", got, minSamplesForWindowMirror)
	}
	if errs := validateSamplerCapacity(rc); len(errs) != 0 {
		t.Fatalf("validateSamplerCapacity() = %v, want none for a sufficient capacity", errs)
	}

	// A zero window forces the otherwise unreachable defensive branch.
	rc.VMPolicy.Advanced.ScaleDownWindow = 0
	if got := rc.SamplerCapacity(); got != 1 {
		t.Fatalf("test setup: SamplerCapacity() = %d, want 1 with a zero window", got)
	}
	errs := validateSamplerCapacity(rc)
	if len(errs) != 1 {
		t.Fatalf("validateSamplerCapacity() = %v, want exactly one error", errs)
	}
	if !strings.Contains(errs[0].Error(), "collection_interval") {
		t.Errorf("error = %v, want it to name collection_interval", errs[0])
	}
}

func TestValidateCollectionIntervalAgainstWindows_rejectsTooLongForShortestWindow(t *testing.T) {
	rc := &ResolvedConfig{
		VMPolicy: ResolvedPolicy{Advanced: ResolvedAdvanced{
			ScaleUpWindow: 5 * time.Minute, ScaleDownWindow: 30 * time.Minute,
		}},
		Runtime: ResolvedRuntime{CollectionInterval: 20 * time.Minute},
	}
	// Buffer capacity follows the 30-minute down window, but cadence still breaks the five-minute up window.
	if got := rc.SamplerCapacity(); got < minSamplesForWindowMirror {
		t.Fatalf("test setup: SamplerCapacity() = %d, want >= %d — this test is specifically about SamplerCapacity NOT catching the bug", got, minSamplesForWindowMirror)
	}
	if errs := validateSamplerCapacity(rc); len(errs) != 0 {
		t.Fatalf("test setup: validateSamplerCapacity() = %v, want none — confirms it alone would have passed this config", errs)
	}

	errs := validateCollectionIntervalAgainstWindows(rc)
	if len(errs) != 1 {
		t.Fatalf("validateCollectionIntervalAgainstWindows() = %v, want exactly one error", errs)
	}
	if !strings.Contains(errs[0].Error(), "scale_up_window") {
		t.Errorf("error = %v, want it to name scale_up_window", errs[0])
	}
}

func TestValidateCollectionIntervalAgainstWindows_catchesPerDiskOverride(t *testing.T) {
	// Only the per-disk override is shorter than the configured cadence.
	rc := &ResolvedConfig{
		VMPolicy: ResolvedPolicy{Advanced: ResolvedAdvanced{
			ScaleUpWindow: 30 * time.Minute, ScaleDownWindow: 60 * time.Minute,
		}},
		DiskPolicies: map[string]ResolvedPolicy{
			"fastdisk": {Advanced: ResolvedAdvanced{ScaleUpWindow: 2 * time.Minute, ScaleDownWindow: 60 * time.Minute}},
		},
		Runtime: ResolvedRuntime{CollectionInterval: 5 * time.Minute},
	}
	errs := validateCollectionIntervalAgainstWindows(rc)
	if len(errs) != 1 {
		t.Fatalf("validateCollectionIntervalAgainstWindows() = %v, want exactly one error", errs)
	}
	if !strings.Contains(errs[0].Error(), "fastdisk") || !strings.Contains(errs[0].Error(), "scale_up_window") {
		t.Errorf("error = %v, want it to name disk %q and scale_up_window", errs[0], "fastdisk")
	}
}

func TestValidateCollectionIntervalAgainstWindows_equalToWindowIsRejected(t *testing.T) {
	// Equal cadence yields only two boundary samples, below the three-sample minimum.
	rc := &ResolvedConfig{
		VMPolicy: ResolvedPolicy{Advanced: ResolvedAdvanced{
			ScaleUpWindow: 5 * time.Minute, ScaleDownWindow: 30 * time.Minute,
		}},
		Runtime: ResolvedRuntime{CollectionInterval: 5 * time.Minute}, // equal to scale_up_window
	}
	errs := validateCollectionIntervalAgainstWindows(rc)
	if len(errs) != 1 {
		t.Fatalf("validateCollectionIntervalAgainstWindows() = %v, want exactly one error — interval equal to a window can never fill it", errs)
	}
	if !strings.Contains(errs[0].Error(), "scale_up_window") {
		t.Errorf("error = %v, want it to name scale_up_window", errs[0])
	}
}

func TestValidateCollectionIntervalAgainstWindows_strictlyShorterThanWindowIsValid(t *testing.T) {
	// A strictly shorter cadence can supply the third sample.
	rc := &ResolvedConfig{
		VMPolicy: ResolvedPolicy{Advanced: ResolvedAdvanced{
			ScaleUpWindow: 5 * time.Minute, ScaleDownWindow: 30 * time.Minute,
		}},
		Runtime: ResolvedRuntime{CollectionInterval: 4 * time.Minute}, // shorter than scale_up_window
	}
	if errs := validateCollectionIntervalAgainstWindows(rc); len(errs) != 0 {
		t.Errorf("validateCollectionIntervalAgainstWindows() = %v, want none — a strictly shorter interval is valid", errs)
	}
}

func TestValidate_platformSourceWithKeyConfigured(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/monitoring-key"
	writeFile(t, keyPath, "fake-api-key")
	t.Setenv(monitoringAPIKeyPathEnv, keyPath)

	cfg := validConfig()
	cfg.Runtime.Metrics.Source = "platform"

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() = %v, want nil once %s points to an existing file", err, monitoringAPIKeyPathEnv)
	}
}
