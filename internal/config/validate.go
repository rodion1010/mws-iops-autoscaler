package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
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
	errs = append(errs, validateResolvedRuntime("runtime", resolved.Runtime)...)
	errs = append(errs, validateSamplerCapacity(resolved)...)

	return errors.Join(errs...)
}

// minSamplesForWindowMirror duplicates internal/analyzer's own
// minSamplesForWindow constant (config must not import analyzer —
// design.md §3.3's decoupling, the same tradeoff internal/decider already
// accepts for billingStepIOPS). Kept in sync by hand; both are
// calibration-pending together (design.md §8.5).
const minSamplesForWindowMirror = 3

// validateSamplerCapacity rejects a config whose runtime.metrics.collection_interval
// is too long relative to the configured windows for the Sampler's ring
// buffer to ever hold minSamplesForWindowMirror samples (issue #6,
// reopened: "cross-validate collection cadence/windows/minimum samples so
// an accepted config can eventually become evaluable"). It calls the same
// ResolvedConfig.SamplerCapacity the orchestrator uses to size the real
// buffer — one formula, checked here and used there, rather than two
// independently-reasoned-about approximations that could disagree. Below
// this floor, WindowFullUp/WindowFullDown (ADR-010) can never become true
// for any disk, for any window, no matter how long the agent runs: the
// ring buffer would evict down to fewer samples than analyzer.Analyze's
// own minimum before a window could ever fill.
//
// SamplerCapacity's own corrected formula (issue #7, reopened:
// ceil(window/interval) + 1 + samplerCapacitySlackSamples) means this can
// no longer actually fire for any positive window and interval — the
// formula's own minimum output is 1+1+2=4, already above this function's
// floor of 3. Kept anyway as defense in depth (this codebase's established
// pattern — see e.g. Agent's own allowScaleDown double-check): if
// SamplerCapacity's formula or its slack constant ever changes, this stays
// the backstop that catches a config that would otherwise silently never
// become evaluable, without requiring every future change to that formula
// to remember this invariant on its own.
func validateSamplerCapacity(resolved *ResolvedConfig) []error {
	if capacity := resolved.SamplerCapacity(); capacity < minSamplesForWindowMirror {
		return []error{fmt.Errorf(
			"runtime.metrics.collection_interval: %s is too long relative to the configured evaluation windows — "+
				"the sample buffer could only ever hold %d sample(s), but at least %d are required before any window can become evaluable",
			resolved.Runtime.CollectionInterval, capacity, minSamplesForWindowMirror)}
	}
	return nil
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
	if rp.Advanced.CooldownScaleDown > rp.Advanced.ScaleDownWindow {
		errs = append(errs, fmt.Errorf(
			"%s.advanced.cooldown.scale_down: %s is longer than scale_down_window (%s) — the agent could never act, and a restart could not safely forget a longer-than-window cooldown (ADR-013)",
			path, rp.Advanced.CooldownScaleDown, rp.Advanced.ScaleDownWindow))
	}

	errs = append(errs, validateAdvancedRanges(path, rp.Advanced)...)

	return errs
}

// validateAdvancedRanges checks every advanced field against the finite
// range or enum its meaning actually admits (issue #6) — everything here
// previously passed Validate silently, including a threshold of 0 (a gate
// that never opens or a target that permits zero headroom) and negative
// durations/counts.
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

	// finite guards every float check below against NaN/Inf (issue #6,
	// reopened): a bare `v <= 0 || v > 1` silently lets NaN through, since
	// every comparison against NaN is false — `NaN <= 0` is false and
	// `NaN > 1` is false, so the "violation" branch never fires and NaN
	// passes as if it were a valid fraction. Reports its own error and
	// returns false so a caller can skip a range comparison that would be
	// meaningless anyway.
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
		// Strictly positive, not merely non-negative (issue #6, reopened):
		// with a non-negative QueueDepth.Value, a threshold of exactly 0
		// makes sustainedQueueAtOrAbove/sustainedQueueBelow (analyzer.go)
		// trivially true/false on every sample regardless of actual queue
		// depth — the gate stops gating, and utilization_threshold alone
		// would decide saturation.
		errs = append(errs, fmt.Errorf("%s.advanced.saturation.queue_depth_threshold: %v must be greater than zero", path, a.QueueDepthThreshold))
	}

	// Only "relative" is implemented — design.md §2/§9.4 explicitly places
	// absolute latency thresholds out of scope for v1, and no Analyzer
	// code path reads LatencyMode at all today (analyzer.EvaluationPolicy
	// carries no such field). Accepting "absolute" therefore accepted a
	// value the agent silently treats identically to "relative," with
	// nothing to signal the difference (issue #6, reopened).
	if a.LatencyMode != "relative" {
		errs = append(errs, fmt.Errorf("%s.advanced.saturation.latency.mode: %q — only \"relative\" is implemented in this phase (design.md §2/§9.4)", path, a.LatencyMode))
	}
	if finite("saturation.latency.baseline_multiplier", a.LatencyBaselineMultiplier) && a.LatencyBaselineMultiplier <= 1 {
		// > 1, not merely > 0 (issue #6, reopened): the elevated-latency
		// trigger compares observed p95 against baseline.P95 × multiplier
		// (analyzer.go's scaledDuration). A multiplier at or below 1 sets
		// that threshold at or below the baseline itself, so ordinary
		// measurement noise around baseline latency would read as
		// "elevated" continuously.
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

// validateResolvedRuntime checks the runtime block's intervals — all three
// are durations the controller schedules on, so zero or negative silently
// breaks scheduling rather than failing validation.
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

// validateDiskSelection checks disks.mode is one of the two documented
// strategies (docs/configuration.md §4.2) and that only the fields that
// strategy actually uses are set, then checks device_name form (ADR-004)
// and duplicates within whichever list applies.
func validateDiskSelection(d *DisksSpec) []error {
	var errs []error

	switch d.Mode {
	case "auto-discovery":
		if len(d.Selected) > 0 {
			errs = append(errs, fmt.Errorf(
				"disks: mode is %q but disks.selected is set — selected only applies to mode: selected", d.Mode))
		}
	case "selected":
		if len(d.Exclude) > 0 {
			errs = append(errs, fmt.Errorf(
				"disks: mode is %q but disks.exclude is set — exclude only applies to mode: auto-discovery", d.Mode))
		}
		if len(d.Selected) == 0 {
			errs = append(errs, fmt.Errorf("disks.selected: mode is \"selected\" but the list is empty"))
		}
	default:
		errs = append(errs, fmt.Errorf(
			"disks.mode: %q is neither \"auto-discovery\" nor \"selected\"", d.Mode))
	}

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
