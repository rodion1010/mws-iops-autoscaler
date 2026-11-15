//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// TestSmoke_Decisions_ScaleDownAfterQuiet asserts a scale-down after a quiet
// period. The disk's normal resting baseline already
// sits at min_iops, so there is nothing to scale down *from* there — this
// scenario raises the limit directly first (ResetDiskToBaseline against an
// elevated value), then starts the agent against a disk that is idle from
// its first sample.
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

	runAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
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

// TestSmoke_Decisions_NoFlapping drives alternating bursts around the
// threshold and asserts the number of changes stays within what hysteresis
// and cooldown allow.
//
// Each cycle first proves a real scale-up and scale-down happened
// (proveUpDownCycle) before the transition-count bound is checked — an
// all-quiet journal would otherwise satisfy "no more than N transitions"
// vacuously. Two cycles bound the count at 3 (up→down→up→down).
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

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
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
		current := target.BaselineIOPS
		for cycle := 0; cycle < 2; cycle++ {
			up, down, err := proveUpDownCycle(ctx, client, target, current)
			if err != nil {
				return fmt.Errorf("cycle %d: %w", cycle, err)
			}
			t.Logf("cycle %d: %d -> %d -> %d", cycle, current, up, down)
			current = down
		}
		return AssertBoundedFlapping(ctx, target, 3)
	})
}

// proveUpDownCycle drives real load until a scale-up is observed above
// from, then stops it and requires a scale-down below that new value. It
// reuses waitForScaleUp/waitForScaleDown against a Target copy whose
// BaselineIOPS tracks the moving reference each half-cycle starts from.
func proveUpDownCycle(ctx context.Context, client compute.Client, target Target, from int64) (up, down int64, err error) {
	const (
		upTimeout    = 90 * time.Second
		downTimeout  = 150 * time.Second
		pollInterval = 5 * time.Second
	)

	ref := target
	ref.BaselineIOPS = from
	if err := DriveLoad(ctx, target); err != nil {
		return 0, 0, fmt.Errorf("drive load: %w", err)
	}
	up, err = waitForScaleUp(ctx, client, ref, upTimeout, e2eScaleUpWindow, pollInterval, time.Now())
	if err != nil {
		return 0, 0, fmt.Errorf("wait for scale-up: %w", err)
	}

	if _, err := target.Run(ctx, killLoadCmd()); err != nil {
		return 0, 0, fmt.Errorf("stop load: %w", err)
	}
	ref.BaselineIOPS = up
	down, err = waitForScaleDown(ctx, client, ref, downTimeout, e2eScaleDownWindow, pollInterval, time.Now())
	if err != nil {
		return 0, 0, fmt.Errorf("wait for scale-down: %w", err)
	}
	return up, down, nil
}
