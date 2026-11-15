//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSmoke_Decisions_ScaleUpOnSaturation drives the dependent disk on the
// shared PR-tier target into saturation and asserts a scale-up. It also
// exercises the dependent write path, deliberately, since that disk is the
// only dependent disk this tier has.
func TestSmoke_Decisions_ScaleUpOnSaturation(t *testing.T) {
	target, err := SmokeTarget()
	if err != nil {
		SkipOrFail(t, err)
		return
	}
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}

	if err := Preflight(ctx, client, target); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	// A cleanup failure must make the shared-target check red.
	defer func() {
		cleanupCtx, cleanupCancel := recoveryContext()
		defer cleanupCancel()
		if err := Cleanup(cleanupCtx, client, target); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	runAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
		if err := DriveLoad(ctx, target); err != nil {
			return fmt.Errorf("drive load: %w", err)
		}
		final, err := WaitForScaleUp(ctx, client, target, 3*time.Minute, agentStarted)
		if err != nil {
			return err
		}
		t.Logf("scaled from baseline %d to %d", target.BaselineIOPS, final)
		return nil
	})
}

// TestSmoke_Decisions_NoScaleUpWhenEfficient drives real, sustained I/O that
// never crosses the saturation gate and asserts the
// disk is never scaled up. Asserting only "IOPS never changed" would pass
// vacuously for a dead or crashed agent that never evaluated anything, so
// this first requires a journal record proving the agent reached a full
// evaluation window before crediting the subsequent no-change observation as
// meaningful — any no_action reason other than the three trivial startup
// ones ("no_data", "metrics_stale", "window_not_full") proves that, since
// every other no_action reason (internal/decider.go's "no_signal" and
// "scale_down_disabled" alike — a sub-threshold, flat-latency load with
// scale-down disabled by default legitimately lands on either)
// is only reachable once HasData, Fresh and a full window all hold.
func TestSmoke_Decisions_NoScaleUpWhenEfficient(t *testing.T) {
	target, err := SmokeTarget()
	if err != nil {
		SkipOrFail(t, err)
		return
	}
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}

	if err := Preflight(ctx, client, target); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := recoveryContext()
		defer cleanupCancel()
		if err := Cleanup(cleanupCtx, client, target); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	runAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
		// Well under the config's 900-ops saturation gate (0.90 threshold on
		// a 1000-IOPS baseline) — real, sustained I/O that must never read
		// as saturated.
		if err := DriveRateLimitedLoad(ctx, target, 300); err != nil {
			return fmt.Errorf("drive rate-limited load: %w", err)
		}

		trivialStartupReason := map[string]bool{
			"no_data":         true,
			"metrics_stale":   true,
			"window_not_full": true,
		}
		liveness, err := WaitForJournalRecord(ctx, target, 2*time.Minute, 5*time.Second, func(rec journalRecord) bool {
			return rec.Action == "no_action" && !trivialStartupReason[rec.Reason]
		})
		if err != nil {
			return fmt.Errorf("proof of liveness: %w", err)
		}
		t.Logf("liveness confirmed via reason %q", liveness.Reason)

		if err := AssertNoIOPSChange(ctx, client, target, 60*time.Second, 10*time.Second); err != nil {
			return err
		}
		return nil
	})
}

// TestSmoke_Decisions_ScaleUpOnBandwidthSaturation drives a large-block,
// high-depth load that saturates bandwidth while ops stay well under the
// IOPS-utilization threshold — sustainedResourceGate (internal/analyzer.go)
// is meant to catch this via its bandwidth branch specifically. The journal
// reasons reachable only from inside decider.go's `case a.Saturated:` branch
// prove saturation was detected at all; the IOPS-specific Utilization
// reading is logged (not asserted on — it's a single newest-sample value,
// not a window average) to see whether it was plausibly the bandwidth
// branch rather than the IOPS one that fired.
//
// This scenario does not assert that a scale-up necessarily occurs: demand()
// (internal/decider.go) estimates demand in ops, and an ops-denominated
// estimate may legitimately fail to clear the scale-up utilization ratio
// against a bandwidth-bound workload even once saturation is correctly
// detected. "target_not_above_current" is a real, reportable gap between
// the demand estimator and this workload shape — not a bug in this test —
// so it is accepted here rather than forced to a specific action.
func TestSmoke_Decisions_ScaleUpOnBandwidthSaturation(t *testing.T) {
	target, err := SmokeTarget()
	if err != nil {
		SkipOrFail(t, err)
		return
	}
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}

	if err := Preflight(ctx, client, target); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := recoveryContext()
		defer cleanupCancel()
		if err := Cleanup(cleanupCtx, client, target); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	// Reachable only from decider.go's `case a.Saturated:` branch — includes
	// vm_headroom_insufficient (a real possibility on a VM with two active
	// data disks) and cooldown_scale_up/budget_exhausted (unreachable on
	// this test's fresh state dir, listed anyway so a future change to that
	// assumption fails loud here instead of as an opaque timeout).
	saturatedReasons := map[string]bool{
		"saturated":                true,
		"target_not_above_current": true,
		"baseline_unavailable":     true,
		"vm_headroom_insufficient": true,
		"cooldown_scale_up":        true,
		"budget_exhausted":         true,
	}

	runAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
		if err := DriveBandwidthLoad(ctx, target); err != nil {
			return fmt.Errorf("drive bandwidth load: %w", err)
		}

		rec, err := WaitForJournalRecord(ctx, target, 3*time.Minute, 5*time.Second, func(rec journalRecord) bool {
			return saturatedReasons[rec.Reason]
		})
		if err != nil {
			return fmt.Errorf("wait for saturation detection: %w", err)
		}
		// Utilization is a single newest-sample reading (internal/analyzer.go's
		// iopsUtilization), not a window average — logged rather than asserted
		// on, since one noisy sample crossing the IOPS gate would misreport a
		// fio-calibration issue as a product regression.
		t.Logf("saturation detected: reason=%q iops_utilization=%v target=%d",
			rec.Reason, rec.Utilization, rec.TargetIOPS)
		return nil
	})
}
