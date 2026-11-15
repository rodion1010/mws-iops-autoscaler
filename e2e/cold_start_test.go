//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSmoke_Decisions_ScaleUpFromColdStart drives load before the agent
// starts, so its first samples already show saturation with no established
// baseline. The closed-workload branch of demand() requires
// BaselineEstablished, which forms only once samples arrive.
//
// DriveLoad must stay before runAgentScenario: issuing it inside the callback
// is TestSmoke_Decisions_ScaleUpOnSaturation's ordering, not this scenario's.
func TestSmoke_Decisions_ScaleUpFromColdStart(t *testing.T) {
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
	defer func() {
		cleanupCtx, cleanupCancel := recoveryContext()
		defer cleanupCancel()
		if err := Cleanup(cleanupCtx, client, target); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	// Deliberately before the agent starts — see the doc comment above.
	if err := DriveLoad(ctx, target); err != nil {
		t.Fatalf("drive load: %v", err)
	}

	runAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
		final, err := WaitForScaleUp(ctx, client, target, 4*time.Minute, agentStarted)
		if err != nil {
			return fmt.Errorf("wait for scale-up from cold start: %w", err)
		}
		t.Logf("scaled from baseline %d to %d despite load already present at agent start", target.BaselineIOPS, final)
		return nil
	})
}
