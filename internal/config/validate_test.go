package config

import (
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
			Exclude: []string{"bootdisk"},
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
			name: "device_name with the mws- prefix",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Exclude = nil
				c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "mws-datadisk"}}
			},
			wantSub: "mws- prefix",
		},
		{
			name: "duplicate device_name within selected",
			mutate: func(c *Config) {
				c.Disks.Mode = "selected"
				c.Disks.Exclude = nil
				c.Disks.Selected = []SelectedDiskSpec{{DeviceName: "datadisk"}, {DeviceName: "datadisk"}}
			},
			wantSub: "listed more than once",
		},
		{
			name: "duplicate device_name within exclude",
			mutate: func(c *Config) {
				c.Disks.Exclude = []string{"bootdisk", "bootdisk"}
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
				// c.Disks.Exclude is already ["bootdisk"] from validConfig.
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Deterministic regardless of the ambient environment — the
			// "platform source without a key" case relies on this being unset.
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
			DeviceName: "datadisk",
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

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

func durationPtr(s string) *Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	dd := Duration(d)
	return &dd
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
