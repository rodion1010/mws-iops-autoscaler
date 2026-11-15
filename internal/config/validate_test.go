package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		Version: 1,
		Mode:    "local",
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
	c.Disks.Policies = map[string]*PolicySpec{"mws-datadisk": nil}

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
		c.Disks.Policies = map[string]*PolicySpec{"datadisk": nil}

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
			name: "device_name as a kernel device path",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Exclude = nil
				c.Disks.Policies = map[string]*PolicySpec{"/dev/vdb": nil}
			},
			wantSub: "kernel device path",
		},
		{
			name: "device_name matching the exact value VM status reports is accepted",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Exclude = nil
				c.Disks.Policies = map[string]*PolicySpec{"mws-datadisk": nil}
			},
			wantSub: "",
		},
		{
			name: "duplicate device_name within exclude",
			mutate: func(c *Config) {
				c.Disks.Exclude = []string{"mws-bootdisk", "mws-bootdisk"}
			},
			wantSub: "listed more than once",
		},
		{
			name: "platform source is not implemented",
			mutate: func(c *Config) {
				c.Runtime.Metrics.Source = "platform"
			},
			wantSub: "not implemented yet",
		},
		{
			name: "scale_down_window shorter than scale_up_window",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Evaluation: &EvaluationSpec{
						ScaleUpWindow:   durationPtr("10m"),
						ScaleDownWindow: durationPtr("5m"),
					},
				}
			},
			wantSub: "invites flapping",
		},
		{
			name: "scale_down_utilization not below scale_up_utilization",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Target: &TargetSpec{
						ScaleUpUtilization:   floatPtr(0.70),
						ScaleDownUtilization: floatPtr(0.70),
					},
				}
			},
			wantSub: "invites flapping",
		},
		{
			name: "scale_down_utilization above scale_up_utilization",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Target: &TargetSpec{
						ScaleUpUtilization:   floatPtr(0.70),
						ScaleDownUtilization: floatPtr(0.85),
					},
				}
			},
			wantSub: "invites flapping",
		},
		{
			// A completed downscale here lands at 0.80 utilization, above
			// the 0.70 saturation gate — immediately a saturated candidate
			// again, even though scale_down (0.80) is below scale_up
			// (0.90) and the old check alone would have passed this.
			name: "scale_down_utilization above the saturation gate despite being below scale_up_utilization",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{UtilizationThreshold: floatPtr(0.70)},
					Target: &TargetSpec{
						ScaleUpUtilization:   floatPtr(0.90),
						ScaleDownUtilization: floatPtr(0.80),
					},
				}
			},
			wantSub: "scale_down_utilization: 0.8 is not below saturation.utilization_threshold",
		},
		{
			name: "scale_up_utilization at or above the saturation gate",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{UtilizationThreshold: floatPtr(0.90)},
					Target: &TargetSpec{
						ScaleUpUtilization:   floatPtr(0.90),
						ScaleDownUtilization: floatPtr(0.70),
					},
				}
			},
			wantSub: "scale_up_utilization: 0.9 is not below saturation.utilization_threshold",
		},
		{
			name: "cooldown.scale_up longer than scale_up_window",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
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
			name: "mode: auto-discovery with a policy for an excluded disk",
			mutate: func(c *Config) {
				c.Disks.Policies = map[string]*PolicySpec{"mws-bootdisk": nil}
			},
			wantSub: "both excluded and given a policy",
		},
		{
			name: "mode: selected with exclude set",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Policies = map[string]*PolicySpec{"datadisk": nil}
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
			wantSub: "mode is \"selected\" but no disk is listed",
		},
		{
			name: "negative scale_up_window",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Evaluation: &EvaluationSpec{ScaleUpWindow: negativeDurationPtr(t, "5m")},
				}
			},
			wantSub: "evaluation.scale_up_window",
		},
		{
			name: "negative cooldown.scale_down",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Cooldown: &CooldownSpec{ScaleDown: negativeDurationPtr(t, "5m")},
				}
			},
			wantSub: "cooldown.scale_down",
		},
		{
			name: "utilization_threshold of zero",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{UtilizationThreshold: floatPtr(0)},
				}
			},
			wantSub: "saturation.utilization_threshold",
		},
		{
			name: "utilization_threshold above 1",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{UtilizationThreshold: floatPtr(1.5)},
				}
			},
			wantSub: "saturation.utilization_threshold",
		},
		{
			name: "scale_up_utilization of zero",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Target: &TargetSpec{ScaleUpUtilization: floatPtr(0)},
				}
			},
			wantSub: "target.scale_up_utilization",
		},
		{
			name: "negative queue_depth_threshold",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{QueueDepthThreshold: floatPtr(-1)},
				}
			},
			wantSub: "saturation.queue_depth_threshold",
		},
		{
			name: "unknown latency.mode",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{Latency: &LatencySpec{Mode: strPtr("eventual")}},
				}
			},
			wantSub: "latency.mode",
		},
		{
			name: "zero latency.baseline_multiplier",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{Latency: &LatencySpec{BaselineMultiplier: floatPtr(0)}},
				}
			},
			wantSub: "latency.baseline_multiplier",
		},
		{
			name: "min_change_iops not a multiple of 100",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Step: &StepSpec{MinChangeIOPS: intPtr(150)},
				}
			},
			wantSub: "step.min_change_iops",
		},
		{
			name: "zero min_change_iops",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Step: &StepSpec{MinChangeIOPS: intPtr(0)},
				}
			},
			wantSub: "step.min_change_iops",
		},
		{
			name: "max_scale_up_ratio at or below 1",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Step: &StepSpec{MaxScaleUpRatio: floatPtr(1)},
				}
			},
			wantSub: "step.max_scale_up_ratio",
		},
		{
			name: "max_scale_down_ratio at or above 1",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Step: &StepSpec{MaxScaleDownRatio: floatPtr(1)},
				}
			},
			wantSub: "step.max_scale_down_ratio",
		},
		{
			name: "zero max_changes_per_24h",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Budget: &BudgetSpec{MaxChangesPer24h: intPtr(0)},
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
					Saturation: &SaturationSpec{Latency: &LatencySpec{Mode: strPtr("absolute")}},
				}
			},
			wantSub: "latency.mode",
		},
		{
			name: "NaN utilization_threshold rejected as non-finite, not silently passed",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{UtilizationThreshold: floatPtr(math.NaN())},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "+Inf scale_up_utilization rejected as non-finite",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Target: &TargetSpec{ScaleUpUtilization: floatPtr(math.Inf(1))},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "NaN queue_depth_threshold rejected as non-finite",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{QueueDepthThreshold: floatPtr(math.NaN())},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "queue_depth_threshold of exactly zero rejected — gate must not degenerate to always-true",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{QueueDepthThreshold: floatPtr(0)},
				}
			},
			wantSub: "saturation.queue_depth_threshold",
		},
		{
			name: "queue_depth_threshold just above zero is valid",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{QueueDepthThreshold: floatPtr(0.1)},
				}
			},
			wantSub: "",
		},
		{
			name: "latency.baseline_multiplier of exactly 1 rejected — threshold would equal the baseline itself",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{Latency: &LatencySpec{BaselineMultiplier: floatPtr(1)}},
				}
			},
			wantSub: "latency.baseline_multiplier",
		},
		{
			name: "latency.baseline_multiplier just above 1 is valid",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Saturation: &SaturationSpec{Latency: &LatencySpec{BaselineMultiplier: floatPtr(1.01)}},
				}
			},
			wantSub: "",
		},
		{
			name: "NaN max_scale_up_ratio rejected as non-finite",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Step: &StepSpec{MaxScaleUpRatio: floatPtr(math.NaN())},
				}
			},
			wantSub: "not a finite number",
		},
		{
			name: "Inf max_scale_down_ratio rejected as non-finite",
			mutate: func(c *Config) {
				c.Policy.Advanced = &AdvancedSpec{
					Step: &StepSpec{MaxScaleDownRatio: floatPtr(math.Inf(-1))},
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

// A per-disk advanced override takes effect on its own; there is no separate
// opt-in flag to repeat at every level.
func TestValidate_perDiskAdvancedOverrideNeedsNoOptIn(t *testing.T) {
	cfg := validConfig()
	cfg.Disks.Policies = map[string]*PolicySpec{
		"mws-datadisk": {Advanced: &AdvancedSpec{Step: &StepSpec{MinChangeIOPS: intPtr(500)}}},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if got := Resolve(cfg).DiskPolicies["mws-datadisk"].Advanced.MinChangeIOPS; got != 500 {
		t.Errorf("resolved min_change_iops = %d, want 500", got)
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

// --validate-config must not accept a source the agent refuses at startup,
// whatever the environment holds.
func TestValidate_platformSourceRejectedEvenWithKeyConfigured(t *testing.T) {
	keyPath := t.TempDir() + "/monitoring-key"
	writeFile(t, keyPath, "fake-api-key")
	t.Setenv("MWS_MONITORING_API_KEY_PATH", keyPath)

	cfg := validConfig()
	cfg.Runtime.Metrics.Source = "platform"

	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "not implemented yet") {
		t.Fatalf("Validate() = %v, want \"platform\" rejected as not implemented", err)
	}
}
