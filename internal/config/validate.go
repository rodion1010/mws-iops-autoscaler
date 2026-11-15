package config

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Validate returns every configuration violation in deterministic, field-qualified form.
// Unknown YAML keys are rejected earlier by strict decoding.
func Validate(cfg *Config) error {
	var errs []error

	if cfg.Mode != "local" {
		return fmt.Errorf("mode: %q is not supported in this build — only local mode is implemented", cfg.Mode)
	}

	errs = append(errs, validateDiskSelection(&cfg.Disks)...)
	errs = append(errs, validateMetricsSource(cfg)...)

	// Cross-field rules use resolved values after inheritance.
	resolved := Resolve(cfg)
	errs = append(errs, validateResolvedPolicy("policy", resolved.VMPolicy)...)
	for _, name := range cfg.Disks.PolicyNames() {
		errs = append(errs, validateResolvedPolicy(fmt.Sprintf("disks.policies[%s]", name), resolved.DiskPolicies[name])...)
	}
	errs = append(errs, validateResolvedRuntime("runtime", resolved.Runtime)...)
	errs = append(errs, validateSamplerCapacity(resolved)...)
	errs = append(errs, validateCollectionIntervalAgainstWindows(resolved)...)

	return errors.Join(errs...)
}

// minSamplesForWindowMirror mirrors analyzer's readiness floor without coupling packages.
// Keep it synchronized with analyzer.minSamplesForWindow.
const minSamplesForWindowMirror = 3

// validateSamplerCapacity ensures the shared ring can retain the analyzer's minimum sample count.
// The current capacity formula always satisfies this, so the check is a backstop against future changes.
func validateSamplerCapacity(resolved *ResolvedConfig) []error {
	if capacity := resolved.SamplerCapacity(); capacity < minSamplesForWindowMirror {
		return []error{fmt.Errorf(
			"runtime.metrics.collection_interval: %s is too long relative to the configured evaluation windows — "+
				"the sample buffer could only ever hold %d sample(s), but at least %d are required before any window can become evaluable",
			resolved.Runtime.CollectionInterval, capacity, minSamplesForWindowMirror)}
	}
	return nil
}

// validateCollectionIntervalAgainstWindows checks every resolved window independently.
// A buffer sized for a longer window does not make a shorter window evaluable when collection
// cadence is too slow.
func validateCollectionIntervalAgainstWindows(resolved *ResolvedConfig) []error {
	interval := resolved.Runtime.CollectionInterval
	if interval <= 0 {
		return nil // validateResolvedRuntime already rejects this
	}

	var errs []error
	check := func(path, field string, window time.Duration) {
		if window <= 0 {
			return // validateAdvancedRanges already rejects this
		}
		// Equality still yields only two boundary samples; analyzer requires three.
		// The interval must therefore be strictly shorter than every window.
		if interval >= window {
			errs = append(errs, fmt.Errorf(
				"runtime.metrics.collection_interval: %s is not shorter than %s.advanced.evaluation.%s (%s) — "+
					"at that cadence fewer than %d samples can ever fall within the window, so it could never become evaluable",
				interval, path, field, window, minSamplesForWindowMirror))
		}
	}

	check("policy", "scale_up_window", resolved.VMPolicy.Advanced.ScaleUpWindow)
	check("policy", "scale_down_window", resolved.VMPolicy.Advanced.ScaleDownWindow)

	// Stable ordering makes diagnostics and tests deterministic.
	names := make([]string, 0, len(resolved.DiskPolicies))
	for name := range resolved.DiskPolicies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := resolved.DiskPolicies[name]
		path := fmt.Sprintf("disks.policies[%s]", name)
		check(path, "scale_up_window", p.Advanced.ScaleUpWindow)
		check(path, "scale_down_window", p.Advanced.ScaleDownWindow)
	}

	return errs
}

// validateResolvedPolicy checks rules that require inherited values.
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
	// Saturation is gated by utilization_threshold, not by either target
	// ratio — a completed scale-up or scale-down leaves utilization at
	// exactly its own target ratio, so each must sit below the threshold
	// or that change would immediately clear the saturation gate again.
	if rp.Advanced.ScaleUpUtilization >= rp.Advanced.UtilizationThreshold {
		errs = append(errs, fmt.Errorf(
			"%s.advanced.target.scale_up_utilization: %v is not below saturation.utilization_threshold (%v) — "+
				"a completed scale-up would immediately clear the saturation gate again, inviting back-to-back changes",
			path, rp.Advanced.ScaleUpUtilization, rp.Advanced.UtilizationThreshold))
	}
	if rp.Advanced.ScaleDownUtilization >= rp.Advanced.UtilizationThreshold {
		errs = append(errs, fmt.Errorf(
			"%s.advanced.target.scale_down_utilization: %v is not below saturation.utilization_threshold (%v) — "+
				"a completed scale-down would immediately clear the saturation gate again, defeating the headroom it's meant to leave",
			path, rp.Advanced.ScaleDownUtilization, rp.Advanced.UtilizationThreshold))
	}
	if rp.Advanced.ScaleDownUtilization >= rp.Advanced.ScaleUpUtilization {
		errs = append(errs, fmt.Errorf(
			"%s.advanced.target: scale_down_utilization (%v) is not below scale_up_utilization (%v) — invites flapping",
			path, rp.Advanced.ScaleDownUtilization, rp.Advanced.ScaleUpUtilization))
	}
	if rp.Advanced.CooldownScaleUp > rp.Advanced.ScaleUpWindow {
		errs = append(errs, fmt.Errorf(
			"%s.advanced.cooldown.scale_up: %s is longer than scale_up_window (%s) — the agent could never act",
			path, rp.Advanced.CooldownScaleUp, rp.Advanced.ScaleUpWindow))
	}
	if rp.Advanced.CooldownScaleDown > rp.Advanced.ScaleDownWindow {
		errs = append(errs, fmt.Errorf(
			"%s.advanced.cooldown.scale_down: %s is longer than scale_down_window (%s) — the agent could never act, and a restart could not safely forget a longer-than-window cooldown",
			path, rp.Advanced.CooldownScaleDown, rp.Advanced.ScaleDownWindow))
	}

	errs = append(errs, validateAdvancedRanges(path, rp.Advanced)...)

	return errs
}

// validateAdvancedRanges rejects non-finite, out-of-range, or unsupported advanced values.
func validateAdvancedRanges(path string, a ResolvedAdvanced) []error {
	var errs []error

	positive := func(field string, d time.Duration) {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s.advanced.%s: %s must be greater than zero", path, field, d))
		}
	}
	positive("evaluation.scale_up_window", a.ScaleUpWindow)
	positive("evaluation.scale_down_window", a.ScaleDownWindow)

	nonNegativeDuration := func(field string, d time.Duration) {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s.advanced.%s: %s must not be negative", path, field, d))
		}
	}
	nonNegativeDuration("cooldown.scale_up", a.CooldownScaleUp)
	nonNegativeDuration("cooldown.scale_down", a.CooldownScaleDown)

	// Range comparisons do not reject NaN, so guard finiteness first.
	finite := func(field string, v float64) bool {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			errs = append(errs, fmt.Errorf("%s.advanced.%s: %v is not a finite number", path, field, v))
			return false
		}
		return true
	}

	fraction := func(field string, v float64) {
		if !finite(field, v) {
			return
		}
		if v <= 0 || v > 1 {
			errs = append(errs, fmt.Errorf("%s.advanced.%s: %v must be greater than 0 and at most 1", path, field, v))
		}
	}
	fraction("saturation.utilization_threshold", a.UtilizationThreshold)
	fraction("target.scale_up_utilization", a.ScaleUpUtilization)
	fraction("target.scale_down_utilization", a.ScaleDownUtilization)

	if finite("saturation.queue_depth_threshold", a.QueueDepthThreshold) && a.QueueDepthThreshold <= 0 {
		// Zero would make the queue gate trivially true or false for every sample.
		errs = append(errs, fmt.Errorf("%s.advanced.saturation.queue_depth_threshold: %v must be greater than zero", path, a.QueueDepthThreshold))
	}

	// Relative latency is the only implemented mode; accepting others would silently misconfigure behavior.
	if a.LatencyMode != "relative" {
		errs = append(errs, fmt.Errorf("%s.advanced.saturation.latency.mode: %q — only \"relative\" is implemented", path, a.LatencyMode))
	}
	if finite("saturation.latency.baseline_multiplier", a.LatencyBaselineMultiplier) && a.LatencyBaselineMultiplier <= 1 {
		// Values at or below one classify baseline noise as continuously elevated.
		errs = append(errs, fmt.Errorf("%s.advanced.saturation.latency.baseline_multiplier: %v must be greater than 1", path, a.LatencyBaselineMultiplier))
	}

	if a.MinChangeIOPS <= 0 {
		errs = append(errs, fmt.Errorf("%s.advanced.step.min_change_iops: %d must be greater than zero", path, a.MinChangeIOPS))
	} else if a.MinChangeIOPS%BillingStepIOPS != 0 {
		errs = append(errs, fmt.Errorf("%s.advanced.step.min_change_iops: %d is not a multiple of %d", path, a.MinChangeIOPS, BillingStepIOPS))
	}
	if finite("step.max_scale_up_ratio", a.MaxScaleUpRatio) && a.MaxScaleUpRatio <= 1 {
		errs = append(errs, fmt.Errorf("%s.advanced.step.max_scale_up_ratio: %v must be greater than 1 — otherwise it never actually scales up", path, a.MaxScaleUpRatio))
	}
	if finite("step.max_scale_down_ratio", a.MaxScaleDownRatio) && (a.MaxScaleDownRatio <= 0 || a.MaxScaleDownRatio >= 1) {
		errs = append(errs, fmt.Errorf("%s.advanced.step.max_scale_down_ratio: %v must be greater than 0 and less than 1", path, a.MaxScaleDownRatio))
	}

	if a.MaxChangesPer24h <= 0 {
		errs = append(errs, fmt.Errorf("%s.advanced.budget.max_changes_per_24h: %d must be greater than zero", path, a.MaxChangesPer24h))
	}

	return errs
}

// validateResolvedRuntime requires positive controller scheduling intervals.
func validateResolvedRuntime(path string, r ResolvedRuntime) []error {
	var errs []error
	if r.CollectionInterval <= 0 {
		errs = append(errs, fmt.Errorf("%s.metrics.collection_interval: %s must be greater than zero", path, r.CollectionInterval))
	}
	if r.ReconcileInterval <= 0 {
		errs = append(errs, fmt.Errorf("%s.controller.reconcile_interval: %s must be greater than zero", path, r.ReconcileInterval))
	}
	if r.DiscoveryInterval <= 0 {
		errs = append(errs, fmt.Errorf("%s.controller.discovery_interval: %s must be greater than zero", path, r.DiscoveryInterval))
	}
	return errs
}

// validateDiskSelection checks the selection strategy, its applicable fields,
// device-name form, and duplicates.
func validateDiskSelection(d *DisksSpec) []error {
	var errs []error

	switch d.Mode {
	case "auto-discovery":
		// exclude and policies both apply here.
	case "selected":
		if len(d.Exclude) > 0 {
			errs = append(errs, fmt.Errorf(
				"disks: mode is %q but disks.exclude is set — exclude only applies to mode: auto-discovery", d.Mode))
		}
		if len(d.Policies) == 0 {
			errs = append(errs, fmt.Errorf("disks.policies: mode is \"selected\" but no disk is listed"))
		}
	default:
		errs = append(errs, fmt.Errorf(
			"disks.mode: %q is neither \"auto-discovery\" nor \"selected\"", d.Mode))
	}

	// yaml.v3 already rejects a repeated disks.policies key, so only exclude needs a duplicate check.
	seen := make(map[string]bool)
	for i, name := range d.Exclude {
		path := fmt.Sprintf("disks.exclude[%d]", i)
		if err := validateDeviceName(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("%s: device_name %q is listed more than once", path, name))
		}
		seen[name] = true
		if _, ok := d.Policies[name]; ok {
			errs = append(errs, fmt.Errorf("%s: device_name %q is both excluded and given a policy in disks.policies", path, name))
		}
	}
	for _, name := range d.PolicyNames() {
		if err := validateDeviceName(name); err != nil {
			errs = append(errs, fmt.Errorf("disks.policies[%s]: %w", name, err))
		}
	}
	return errs
}

// validateDeviceName rejects a kernel device path and anything that is not
// the exact device_name VM status reports — the same identifier used
// inside Linux. Only resolution against a
// VM's discovered disks (internal/registry) can catch a value that merely
// looks plausible but names no live disk.
func validateDeviceName(name string) error {
	if name == "" {
		return fmt.Errorf("device_name must not be empty")
	}
	if strings.HasPrefix(name, "/dev/") {
		return fmt.Errorf("device_name %q is a kernel device path — use the disk's device_name instead", name)
	}
	if !strings.HasPrefix(name, "mws-") {
		return fmt.Errorf(
			"device_name %q is not the exact value VM status reports — copy it from status.storage.disks[].deviceName, the device_name metric label, or /dev/disk/by-id/",
			name)
	}
	return nil
}

// validateMetricsSource accepts only the implemented source; "platform" ships
// with PlatformSource and is rejected until then.
func validateMetricsSource(cfg *Config) []error {
	switch source := cfg.Runtime.Metrics.Source; source {
	case "", "guest":
		return nil
	case "platform":
		return []error{fmt.Errorf("runtime.metrics.source: \"platform\" is not implemented yet — use \"guest\"")}
	default:
		return []error{fmt.Errorf("runtime.metrics.source: %q is not \"guest\"", source)}
	}
}
