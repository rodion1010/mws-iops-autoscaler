//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSmoke_Decisions_ScaleDownAfterQuiet covers docs/design.md §8.3's
// "scale-down after quiet" row: stop load, wait scale_down_window, assert
// IOPS lowered and never below min_iops. Unlike every scale-up scenario in
// this package, there is nothing to scale down *from* at the disk's normal
// resting baseline (it already sits at min_iops), so this scenario first
// raises the limit directly — via the same production ResetDiskToBaseline
// path Preflight/Cleanup already use, just pointed at an elevated value
// instead of the configured one — then starts the agent against a disk
// that is idle from the very first sample. It never drives any load at
// all, which satisfies "stop load" at least as faithfully as stopping a
// load that was running: either way, the agent's down-window sees nothing
// but quiet.
//
// This is the one scenario in the package that needs allowScaleDown = true,
// so it builds and installs the e2e_scaledown-tagged binary via
// runScaleDownAgentScenario rather than runAgentScenario — see
// cmd/mws-iops-autoscaler/allowscaledown_e2e.go.
func TestSmoke_Decisions_ScaleDownAfterQuiet(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}

	if err := Preflight(ctx, client, target); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	// Cleanup restores target.BaselineIOPS (the configured resting value),
	// not the elevated one below — correct regardless of where this
	// scenario's own scale-down left the disk.
	defer func() {
		cleanupCtx, cleanupCancel := recoveryContext()
		defer cleanupCancel()
		if err := Cleanup(cleanupCtx, client, target); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	// A midpoint between the configured min_iops (1000) and max_iops
	// (10000): comfortably above min_iops so a real scale-down has
	// somewhere to go, comfortably below the VM/platform ceiling so this
	// step cannot itself race a headroom guard.
	const elevatedIOPS = 3000
	elevated := target
	elevated.BaselineIOPS = elevatedIOPS
	if err := ResetDiskToBaseline(ctx, client, elevated); err != nil {
		t.Fatalf("elevate disk to %d before the scenario starts: %v", elevatedIOPS, err)
	}

	runScaleDownAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
		final, err := WaitForScaleDown(ctx, client, elevated, 3*time.Minute, agentStarted)
		if err != nil {
			return err
		}
		if final < 1000 {
			return fmt.Errorf("scaled down to %d, want >= 1000 (the configured min_iops floor)", final)
		}
		t.Logf("scaled down from elevated %d to %d", elevatedIOPS, final)
		return nil
	})
}

// TestSmoke_Decisions_NoFlapping covers docs/design.md §8.3's "no flapping"
// row: alternating bursts around the threshold, assert changes <= expected
// given hysteresis and cooldown. It drives two up/down cycles — a load
// above the saturation gate held for longer than scale_up_window, then
// quiet held for longer than scale_down_window, twice — and asserts the
// resulting scale_up/scale_down actuations in the journal alternate no more
// than 3 times (AssertBoundedFlapping), the ceiling two independent
// up-then-down cycles can produce if each period's own threshold genuinely
// crosses and nothing is suppressed — not a tight prediction of exactly
// what will happen, since cooldown, the 24h budget, or the demand
// estimator's ops-only regime (docs/design.md §4.5, same honestly-disclosed
// gap TestSmoke_Decisions_ScaleUpOnBandwidthSaturation already accepts) may
// legitimately suppress some of these actuations outright. A real flapping
// bug — reconciling every cycle and reversing direction each time — would
// produce an order of magnitude more transitions than this ceiling over the
// same ~4 minutes of alternating load, which is what this scenario is
// actually built to catch.
func TestSmoke_Decisions_NoFlapping(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
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

	// Above the harness config's 900-ops saturation gate (0.90 threshold on
	// the 1000-IOPS baseline) — DriveRateLimitedLoad's own doc comment
	// explains the gate's math; this is deliberately on the other side of
	// it.
	const aboveGateIOPS = 950
	// margin bounds each half-cycle comfortably past its own window
	// (e2eScaleUpWindow=30s / e2eScaleDownWindow=60s) without padding the
	// test past what the smoke tier's timeout budget allows.
	const margin = 15 * time.Second

	runScaleDownAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
		for cycle := 0; cycle < 2; cycle++ {
			if err := DriveRateLimitedLoad(ctx, target, aboveGateIOPS); err != nil {
				return fmt.Errorf("cycle %d: drive above-gate load: %w", cycle, err)
			}
			if err := sleepCtx(ctx, e2eScaleUpWindow+margin); err != nil {
				return err
			}
			if _, err := target.Run(ctx, killLoadCmd()); err != nil {
				return fmt.Errorf("cycle %d: stop load: %w", cycle, err)
			}
			if err := sleepCtx(ctx, e2eScaleDownWindow+margin); err != nil {
				return err
			}
		}

		recs, err := journalRecords(ctx, target)
		if err != nil {
			return fmt.Errorf("fetch journal for liveness check: %w", err)
		}
		if len(recs) == 0 {
			return fmt.Errorf("no journal records at all after two full load cycles — the agent cannot have evaluated anything")
		}
		t.Logf("%d journal records observed over the alternating-load pattern", len(recs))

		return AssertBoundedFlapping(ctx, target, 3)
	})
}

// sleepCtx is time.Sleep that still honors ctx cancellation/timeout, the
// same tradeoff every poll loop in this package already makes.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
