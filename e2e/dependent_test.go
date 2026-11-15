//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSmoke_Dependent_DryRunComputedOnly drives the dependent disk to
// saturation under --dry-run and asserts a scale_up decision lands in the
// journal while the live disk is never mutated. Unlike the independent dry
// run, a dependent dry run makes no write call at all: Apply reports intent
// without calling the write endpoint, and the decision is computed only.
func TestSmoke_Dependent_DryRunComputedOnly(t *testing.T) {
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
