//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSmoke_Independent_ScaleUpOnSaturation exercises the independent write
// path (docs/design.md §4.7, disk PATCH) on the persistent PR-tier target's
// independent disk — the write path
// TestSmoke_Decisions_ScaleUpOnSaturation's dependent disk cannot exercise.
// It shares the VM with that test but targets a different, permanently
// attached disk, so the two never contend for the same resource.
func TestSmoke_Independent_ScaleUpOnSaturation(t *testing.T) {
	target, err := SmokeIndependentTarget()
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

// TestSmoke_Independent_DryRunValidateOnly drives the independent disk to
// saturation under --dry-run and asserts both halves of US-013's
// independent-disk contract: the platform's real ValidateOnly verdict is
// reached — a scale_up decision lands in the journal — while the live disk
// is never mutated. This is the one place a server-confirmed dry-run
// verdict can be observed at all: the dependent path has no equivalent
// (docs/design.md §4.7 — UpdateVirtualMachine has no validateOnly).
func TestSmoke_Independent_DryRunValidateOnly(t *testing.T) {
	target, err := SmokeIndependentTarget()
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

	runAgentScenario(t, ctx, target, project, true, func(agentStarted time.Time) error {
		if err := DriveLoad(ctx, target); err != nil {
			return fmt.Errorf("drive load: %w", err)
		}
		if err := WaitForDecision(ctx, target, 3*time.Minute, 5*time.Second, "scale_up"); err != nil {
			return err
		}
		return AssertNoIOPSChange(ctx, client, target, 30*time.Second, 10*time.Second)
	})
}
