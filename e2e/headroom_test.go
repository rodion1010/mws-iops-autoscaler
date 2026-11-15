//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// TestFull_Headroom_UnmanagedSiblingCountsTowardVMCap asserts that an
// unmanaged sibling's real I/O counts toward a managed disk's VM headroom:
// headroom is computed from all other disks, not only managed ones.
// disks.policies names only the data disk, so any observed headroom
// reduction can only come from the unmanaged churn disk.
func TestFull_Headroom_UnmanagedSiblingCountsTowardVMCap(t *testing.T) {
	fix, ok := ensureFullFixture(t)
	if !ok {
		return
	}
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	// Worst case: ~90s preflight (baseline-confirm retries) + 60s wait for
	// attach + 60s wait for the guest device path + ~40s build/install +
	// 180s worst-case poll for a reduced vm_headroom_iops — comfortably
	// under 10 minutes with margin for the compute API calls in between.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	if err := Preflight(ctx, client, fix.dataTarget()); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	// Starts detached regardless of a previous run's outcome, and ends
	// detached so a later test sees the fixture unchanged.
	if err := DetachChurnDisk(ctx, project, fix); err != nil {
		t.Fatalf("ensure churn disk starts detached: %v", err)
	}
	churnTarget := fix.churnTarget()
	defer func() {
		detachCtx, detachCancel := recoveryContext()
		defer detachCancel()
		if _, err := churnTarget.Run(detachCtx, killLoadCmd()); err != nil {
			t.Logf("stop stale fio on churn disk before detach: %v", err)
		}
		if err := DetachChurnDisk(detachCtx, project, fix); err != nil {
			t.Logf("detach churn disk: %v", err)
		}
	}()

	if err := AttachChurnDisk(ctx, project, fix); err != nil {
		t.Fatalf("attach churn disk: %v", err)
	}
	if err := waitForDiskAttached(ctx, client, churnTarget, 60*time.Second, 5*time.Second); err != nil {
		t.Fatalf("wait for churn disk attach: %v", err)
	}
	if err := AssertDiskClass(ctx, client, churnTarget); err != nil {
		t.Fatalf("verify churn disk class after attach: %v", err)
	}
	// The compute API confirming attachment does not mean the guest kernel
	// has created the device node yet.
	if err := waitForGuestDevicePath(ctx, churnTarget, 60*time.Second, 5*time.Second); err != nil {
		t.Fatalf("wait for churn guest device path: %v", err)
	}

	// Driven before the agent starts so the windowed (not instantaneous)
	// headroom p95 has as much of the remaining runtime as possible to fill.
	// Do not move it into the scenario callback below.
	if err := DriveLoad(ctx, churnTarget); err != nil {
		t.Fatalf("drive load on churn disk: %v", err)
	}

	vm, _, err := compute.Discover(ctx, client, fix.VMName)
	if err != nil {
		t.Fatalf("discover VM to read its IOPS pool: %v", err)
	}
	pool := compute.VMIOPSPool(vm)
	// The test environment requires at least 2 vCPU (>= 6000 pool); a smaller
	// reading means VMIOPSPool or VM-type discovery broke, not that the
	// reductionThreshold below needs to shrink to match it.
	if pool < 1000 {
		t.Fatalf("VM IOPS pool = %d, want at least 1000 — VM-type discovery looks broken, not just small", pool)
	}

	disks := SelectedDisksSpec(fix.DataDeviceName)
	err = runAgentScenarioWithDisksErr(ctx, fix.dataTarget(), project, false, disks, discoveryScenarioInterval, func(time.Time) error {
		// Only a meaningful reduction is asserted, not fio's exact throughput
		// (ramp-up and window-fill add slack). Requiring VMHeadroomIOPS > 0
		// excludes a hand-built zero Decision (e.g. journalDiscoveryStale)
		// from satisfying "reduced below the pool" by accident.
		const reductionThreshold = 300
		_, err := WaitForJournalRecord(ctx, fix.dataTarget(), 3*time.Minute, 5*time.Second, func(rec journalRecord) bool {
			return rec.VMHeadroomIOPS > 0 && rec.VMHeadroomIOPS <= pool-reductionThreshold
		})
		if err != nil {
			return fmt.Errorf("no journal record ever showed vm_headroom_iops reduced by at least %d below the %d pool: %w", reductionThreshold, pool, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("VM headroom did not fold in the unmanaged churn disk's consumption: %v", err)
	}
}
