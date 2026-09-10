//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// TestFull_Headroom_UnmanagedSiblingCountsTowardVMCap is E5 (docs/testing.md):
// an unmanaged sibling's real I/O must still count toward a managed disk's
// VM headroom — docs/design.md §4.6 defines headroom from "other disks", not
// "other managed disks". disks.selected names only the data disk, so any
// headroom reduction observed can only have come from the unmanaged churn
// disk.
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
	// See TestFull_Discovery_AttachedDiskManagedWithoutRestart's identical
	// wait: the compute API confirming attachment does not mean the guest
	// kernel has created the device node yet.
	if err := waitForGuestDevicePath(ctx, churnTarget, 60*time.Second, 5*time.Second); err != nil {
		t.Fatalf("wait for churn guest device path: %v", err)
	}

	// Driven before the agent starts so its down-window has as much of this
	// test's remaining runtime as possible to fill — §4.6's p95 is windowed,
	// not instantaneous. Never move this inside the run() callback below
	// (see e2e/cold_start_test.go's identical warning for why sequencing
	// here is load-bearing, not stylistic).
	if err := DriveLoad(ctx, churnTarget); err != nil {
		t.Fatalf("drive load on churn disk: %v", err)
	}

	vm, _, err := compute.Discover(ctx, client, fix.VMName)
	if err != nil {
		t.Fatalf("discover VM to read its IOPS pool: %v", err)
	}
	pool := compute.VMIOPSPool(vm)
	// docs/development.md requires at least 2 vCPU (>= 6000 pool); a smaller
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
