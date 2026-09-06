//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestSmoke_ScaleUpOnPersistentVM exercises the dependent write path on the
// shared PR-tier target.
func TestSmoke_ScaleUpOnPersistentVM(t *testing.T) {
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

	binPath, err := BuildAgentBinary(ctx, "..")
	if err != nil {
		t.Fatalf("build agent binary: %v", err)
	}
	defer os.Remove(binPath)

	if err := InstallAgent(ctx, target, binPath); err != nil {
		t.Fatalf("install agent: %v", err)
	}
	if err := StartAgent(ctx, target, project); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	agentStarted := time.Now()
	defer func() {
		stopCtx, stopCancel := recoveryContext()
		defer stopCancel()
		if err := StopAgent(stopCtx, target); err != nil {
			t.Logf("stop agent: %v", err)
		}
	}()

	if err := DriveLoad(ctx, target); err != nil {
		t.Fatalf("drive load: %v", err)
	}

	final, err := WaitForScaleUp(ctx, client, target, 3*time.Minute, agentStarted)
	if err != nil {
		diagnosticCtx, diagnosticCancel := recoveryContext()
		journal := FetchJournal(diagnosticCtx, target)
		diagnosticCancel()
		t.Fatalf("%v\njournal:\n%s", err, journal)
	}
	t.Logf("scaled from baseline %d to %d", target.BaselineIOPS, final)
}
