package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Validate checks cfg against every rule in docs/configuration.md §8 and
// returns all violations found, joined, or nil if none. Each error names the
// field it complains about, per that section's own requirement.
//
// Unknown YAML keys are not checked here — Load rejects them at decode time
// via strict decoding, before Validate ever sees the document.
func Validate(cfg *Config) error {
	var errs []error

	if cfg.Mode != "local" {
		// Nothing past this point applies: fleet's config shape does not
		// exist in this build (Phase 1 is local-only, PRD §6).
		return fmt.Errorf("mode: %q is not supported in this build (fleet mode ships in Phase 4)", cfg.Mode)
	}

	errs = append(errs, validateAdvancedGate("policy.advanced", cfg.Policy.Advanced)...)
	errs = append(errs, validateDiskSelection(&cfg.Disks)...)

	for i, d := range cfg.Disks.Selected {
		if d.Policy != nil {
			path := fmt.Sprintf("disks.selected[%d:%s].policy.advanced", i, d.DeviceName)
			errs = append(errs, validateAdvancedGate(path, d.Policy.Advanced)...)
		}
	}

	errs = append(errs, validateMetricsSource(cfg)...)

	// Cross-field rules need the fully resolved policy, since a level may
	// inherit the value on either side of the comparison.
	resolved := Resolve(cfg)
	errs = append(errs, validateResolvedPolicy("policy", resolved.VMPolicy)...)
	for name, rp := range resolved.DiskPolicies {
		errs = append(errs, validateResolvedPolicy(fmt.Sprintf("disks.selected[%s].policy", name), rp)...)
	}

	return errors.Join(errs...)
}

// validateResolvedPolicy checks the range and cross-field rules that only
// make sense once inheritance has been applied.
func validateResolvedPolicy(path string, rp ResolvedPolicy) []error {
	var errs []error

	if rp.MinIOPS < PlatformMinIOPS {
		errs = append(errs, fmt.Errorf("%s.limits.min_iops: %d is below the platform minimum %d", path, rp.MinIOPS, PlatformMinIOPS))
	}
	if rp.MaxIOPS > PlatformMaxIOPS {
		errs = append(errs, fmt.Errorf("%s.limits.max_iops: %d is above the platform maximum %d", path, rp.MaxIOPS, PlatformMaxIOPS))
	}
	if rp.MinIOPS > rp.MaxIOPS {
		errs = append(errs, fmt.Errorf("%s.limits: min_iops (%d) is greater than max_iops (%d)", path, rp.MinIOPS, rp.MaxIOPS))
	}
	if rp.MinIOPS%BillingStepIOPS != 0 {
		errs = append(errs, fmt.Errorf("%s.limits.min_iops: %d is not a multiple of %d", path, rp.MinIOPS, BillingStepIOPS))
	}
	if rp.MaxIOPS%BillingStepIOPS != 0 {
		errs = append(errs, fmt.Errorf("%s.limits.max_iops: %d is not a multiple of %d", path, rp.MaxIOPS, BillingStepIOPS))
	}

	if rp.Advanced.ScaleDownWindow < rp.Advanced.ScaleUpWindow {
		errs = append(errs, fmt.Errorf(
			"%s.advanced.evaluation: scale_down_window (%s) is shorter than scale_up_window (%s) — invites flapping",
			path, rp.Advanced.ScaleDownWindow, rp.Advanced.ScaleUpWindow))
	}
	if rp.Advanced.CooldownScaleUp > rp.Advanced.ScaleUpWindow {
		errs = append(errs, fmt.Errorf(
			"%s.advanced.cooldown.scale_up: %s is longer than scale_up_window (%s) — the agent could never act",
			path, rp.Advanced.CooldownScaleUp, rp.Advanced.ScaleUpWindow))
	}

	return errs
}

// validateAdvancedGate rejects a set advanced.* sub-block whose enclosing
// advanced.enabled is not explicitly true (docs/configuration.md §8).
func validateAdvancedGate(path string, a *AdvancedSpec) []error {
	if a == nil {
		return nil
	}
	enabled := a.Enabled != nil && *a.Enabled
	anySet := a.Evaluation != nil || a.Saturation != nil || a.Target != nil ||
		a.Cooldown != nil || a.Step != nil || a.Budget != nil
	if anySet && !enabled {
		return []error{fmt.Errorf("%s: fields are set but advanced.enabled is not true", path)}
	}
	return nil
}

// validateDiskSelection checks device_name form (ADR-004) and duplicates.
func validateDiskSelection(d *DisksSpec) []error {
	var errs []error
	seen := make(map[string]bool)

	check := func(path, name string) {
		if err := validateDeviceName(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("%s: device_name %q is listed more than once", path, name))
		}
		seen[name] = true
	}

	for i, name := range d.Exclude {
		check(fmt.Sprintf("disks.exclude[%d]", i), name)
	}
	for i, sel := range d.Selected {
		check(fmt.Sprintf("disks.selected[%d].device_name", i), sel.DeviceName)
	}
	return errs
}

// validateDeviceName rejects a kernel device path and the mws- prefix
// (docs/configuration.md §4.1, ADR-004).
func validateDeviceName(name string) error {
	if name == "" {
		return fmt.Errorf("device_name must not be empty")
	}
	if strings.HasPrefix(name, "/dev/") {
		return fmt.Errorf("device_name %q is a kernel device path — use the disk's device_name instead (ADR-004)", name)
	}
	if strings.HasPrefix(name, "mws-") {
		return fmt.Errorf("device_name %q carries the mws- prefix — write it unprefixed, as it was set at attach time (ADR-004)", name)
	}
	return nil
}

// validateMetricsSource checks the mode/source combinations from
// docs/configuration.md §8. In local mode, opting into the platform source
// requires a monitoring API key on disk (CLAUDE.md "Monitoring" — a
// different credential from the compute IAM token).
func validateMetricsSource(cfg *Config) []error {
	source := cfg.Runtime.Metrics.Source
	if source == "" {
		source = defaultMetricsSource
	}
	if source != "guest" && source != "platform" {
		return []error{fmt.Errorf("runtime.metrics.source: %q is neither \"guest\" nor \"platform\"", source)}
	}
	if source != "platform" {
		return nil
	}

	path := os.Getenv(monitoringAPIKeyPathEnv)
	if path == "" {
		return []error{fmt.Errorf(
			"runtime.metrics.source: \"platform\" requires a monitoring API key; set %s", monitoringAPIKeyPathEnv)}
	}
	if _, err := os.Stat(path); err != nil {
		return []error{fmt.Errorf(
			"runtime.metrics.source: \"platform\" needs the file named by %s: %w", monitoringAPIKeyPathEnv, err)}
	}
	return nil
}

// monitoringAPIKeyPathEnv names the file holding the monitoring API key
// (CLAUDE.md's own Prometheus scrape example uses a bearer_token_file, not
// an inline value — the compute IAM token has no equivalent, it comes from
// the metadata service instead).
const monitoringAPIKeyPathEnv = "MWS_MONITORING_API_KEY_PATH"
