//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// TestFull_Discovery_AttachedDiskManagedWithoutRestart is A1
// (docs/testing.md's discovery/scope scenario list): auto-discovery picks up
// a disk attached after the agent has already started, within one
// discovery_interval, with no restart. disks.exclude is left empty rather
// than naming the VM's other disks: naming them here would duplicate
// provision.sh's own device-name choices in Go (the exact duplication
// hazard already designed out for the storage-disks entries themselves by
// echoing them verbatim), and would make this scenario cold-start-fatal
// outright if any one of those names ever drifted from what provision.sh
// actually attaches. The scenario's own assertions only ever read the churn
// disk's IOPS, so managing the boot/data/noname disks too (nothing drives
// load against them) does not make the result ambiguous.
func TestFull_Discovery_AttachedDiskManagedWithoutRestart(t *testing.T) {
	fix, ok := ensureFullFixture(t)
	if !ok {
		return
	}
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	// Matches the other full-tier tests' budget (Lifecycle, DeviceNaming):
	// this scenario adds an attach-and-wait phase on top of the same
	// install/start/drive-load/wait-for-scale-up shape they already need 15
	// minutes for.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}

	// The fixture's other full-tier tests share this VM; Preflight resets
	// whatever state a previous test left behind on the data target — the
	// churn disk itself is reset to baseline below, once it is attached.
	if err := Preflight(ctx, client, fix.dataTarget()); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	// Starts detached regardless of a previous run's outcome, and ends
	// detached so a later test sees the fixture unchanged.
	if err := DetachChurnDisk(ctx, project, fix); err != nil {
		t.Fatalf("ensure churn disk starts detached: %v", err)
	}
	defer func() {
		detachCtx, detachCancel := recoveryContext()
		defer detachCancel()
		// The load fio drives below outlives this test (--runtime=600) and
		// keeps running past StopAgent; detaching a disk under active I/O
		// would race the guest, so stop it first.
		if _, err := fix.dataTarget().Run(detachCtx, killLoadCmd()); err != nil {
			t.Logf("stop stale fio before detach: %v", err)
		}
		if err := DetachChurnDisk(detachCtx, project, fix); err != nil {
			t.Logf("detach churn disk: %v", err)
		}
	}()

	churnTarget := fix.churnTarget()
	disks := AutoDiscoveryDisksSpec()

	runAgentScenarioWithDisks(t, ctx, fix.dataTarget(), project, false, disks, discoveryScenarioInterval, func(agentStarted time.Time) error {
		if err := AttachChurnDisk(ctx, project, fix); err != nil {
			return fmt.Errorf("attach churn disk: %w", err)
		}
		if err := waitForDiskAttached(ctx, client, churnTarget, 60*time.Second, 5*time.Second); err != nil {
			return err
		}
		if err := AssertDiskClass(ctx, client, churnTarget); err != nil {
			return fmt.Errorf("verify churn disk class after attach: %w", err)
		}
		// The compute API reporting the attachment does not mean the guest
		// kernel has created the device node yet — DriveLoad's fio would
		// otherwise fail silently against a path that doesn't exist yet,
		// which reads as "the agent never picked up the disk" (a product
		// bug) rather than what it actually is (attach propagation lag).
		if err := waitForGuestDevicePath(ctx, churnTarget, 60*time.Second, 5*time.Second); err != nil {
			return err
		}
		if err := ResetDiskToBaseline(ctx, client, churnTarget); err != nil {
			return fmt.Errorf("reset churn disk to baseline: %w", err)
		}
		if err := DriveLoad(ctx, churnTarget); err != nil {
			return fmt.Errorf("drive load: %w", err)
		}
		final, err := WaitForScaleUp(ctx, client, churnTarget, 3*time.Minute, agentStarted)
		if err != nil {
			return err
		}
		t.Logf("churn disk scaled from baseline %d to %d after a live attach", churnTarget.BaselineIOPS, final)
		return nil
	})
}

// waitForGuestDevicePath polls the guest OS until t's device node exists.
func waitForGuestDevicePath(ctx context.Context, t Target, timeout, pollInterval time.Duration) error {
	guestPath := (compute.StorageDisk{DeviceName: t.DeviceName}).GuestPath()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		_, err := t.Run(ctx, "test -e "+guestPath)
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e: wait for guest device path: %s still missing on %s after %s (last check: %w)", guestPath, t.VMName, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// TestFull_Discovery_ExcludeUnresolvedNameIsFatalAtColdStart is E6's
// cold-start half (docs/testing.md's discovery/scope scenario list):
// disks.exclude naming a device that has never existed on this VM degrades
// registry.Select's result, and discover() treats any degraded name as
// fatal on the very first discovery of the process's life (docs/design.md
// §4.1) — the agent must refuse to manage anything rather than silently
// auto-discover a partial set.
//
// Agent.Run calls discover() synchronously before its reconcile loop ever
// starts (cmd/mws-iops-autoscaler/agent.go), so a fatal cold-start
// discovery makes Run return immediately and main's deferred HTTP shutdown
// follows right behind it — the whole process exits within seconds. That
// makes "refuses to manage anything" observable here as the agent process
// being gone shortly after start — but process-absence alone would pass
// just as well for a crash from an unrelated cause (a bad chmod, a config
// the YAML parser rejects, a dropped SSH session), so this also reads back
// the agent's captured stdout/stderr and requires it to name this exact
// failure, the same way TestSmoke_Decisions_NoScaleUpWhenEfficient requires
// a journal record before crediting "no change" as meaningful rather than
// a dead agent.
//
// E6's other half — an exclude target that resolves at cold start and only
// disappears later, which must degrade without being fatal — shares
// discover()'s decision path with
// TestReconcile_degradesPartiallyWhenASelectedDiskIsRemovedAtRuntime's
// existing unit coverage and is deferred to a later e2e pass (no new
// product code path to prove live).
func TestFull_Discovery_ExcludeUnresolvedNameIsFatalAtColdStart(t *testing.T) {
	target := fullTarget(t)
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	// Preflight's own reset can take up to three ~23s baseline-confirmation
	// attempts, plus building and copying the agent binary, plus the 30s
	// absence poll below and AssertNoIOPSChange's 15s — 3 minutes leaves
	// almost no margin against that worst case; 5 keeps the workflow sum
	// (67 min) under the 70-minute ceiling with no further change needed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	if err := Preflight(ctx, client, target); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	binPath, err := BuildAgentBinary(ctx, "..")
	if err != nil {
		t.Fatalf("build agent binary: %v", err)
	}
	defer os.Remove(binPath)

	// Never a real device on this VM (boot/data/noname/churn all use their
	// own fixed names) — auto-discovery's exclude can never resolve it.
	const neverAttachedName = "e2e-exclude-ghost"
	disks := AutoDiscoveryDisksSpec(neverAttachedName)
	if err := InstallAgentWithDisks(ctx, target, binPath, disks, discoveryScenarioInterval); err != nil {
		t.Fatalf("install agent: %v", err)
	}
	if err := StartAgent(ctx, target, false); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := recoveryContext()
		defer stopCancel()
		if err := stopStaleAgent(stopCtx, target, defaultAgentStopTiming); err != nil {
			t.Logf("stop agent: %v", err)
		}
	}()

	deadline := time.Now().Add(30 * time.Second)
	exited := false
	for time.Now().Before(deadline) {
		out, err := target.Run(ctx, agentAbsentCmd())
		if err != nil {
			t.Fatalf("check agent presence: %v", err)
		}
		absent, err := parseAgentPresence(out)
		if err != nil {
			t.Fatalf("check agent presence: %v", err)
		}
		if absent {
			exited = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	agentLog := FetchAgentLog(ctx, target)
	if !exited {
		t.Fatalf("agent still running after starting with an unresolved disks.exclude name at cold start — want it to exit, refusing to manage anything (docs/design.md §4.1)\nagent log:\n%s", agentLog)
	}
	// Confirms the process exited *for this reason* — a dead agent alone
	// (a crash from a bad chmod, a rejected config, a dropped SSH session)
	// would otherwise pass just as well.
	if !strings.Contains(agentLog, "initial discovery") || !strings.Contains(agentLog, neverAttachedName) {
		t.Fatalf("agent exited, but its log does not show the expected cold-start-fatal discovery failure naming %q:\n%s", neverAttachedName, agentLog)
	}

	if err := AssertNoIOPSChange(ctx, client, target, 15*time.Second, 5*time.Second); err != nil {
		t.Fatalf("a cold-start-fatal agent must apply no change to any disk: %v", err)
	}
}
