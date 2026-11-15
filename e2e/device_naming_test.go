//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestFull_DeviceNaming_NoDeviceNameDefaultsToAlias exercises an attachment
// with no deviceName set at all: the platform must default the guest device
// to mws-{alias} instead of mws-{deviceName}, and the agent must resolve and
// manage it like any other disk. The data disk on the same VM carries an
// explicit deviceName, so both forms run side by side.
func TestFull_DeviceNaming_NoDeviceNameDefaultsToAlias(t *testing.T) {
	target := fullNoDeviceNameTarget(t)
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	// The VM is shared with other full-tier tests; Preflight resets any
	// state a previous test left behind.
	if err := Preflight(ctx, client, target); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	runAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
		// DriveLoad targets /dev/disk/by-id/mws-{DeviceName}, which only
		// exists if the platform really did default it to mws-{alias}.
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
