//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// isVMPoolCapRecord matches a vm_cap_reached decision whose ceiling was the
// VM pool itself: headroomCeiling also folds in policy.max_iops, and a disk
// pinned there reports the same action with headroom still above current.
func isVMPoolCapRecord(rec journalRecord) bool {
	return rec.Action == "vm_cap_reached" && rec.Reason == "vm_headroom_insufficient" &&
		rec.VMHeadroomIOPS > 0 && rec.VMHeadroomIOPS <= rec.CurrentIOPS
}

// TestSmoke_Decisions_VMCapReachedUnderAggregateLoad covers docs/design.md
// §8.3's "VM cap detection" row: both managed smoke disks under deep load
// together exceed the VM's IOPS pool, so the agent must report
// vm_cap_reached and leave the capped disk's limit alone.
//
// Both disks are first raised to just over half the pool each. Their sum
// oversubscribes the pool, while a disk throttled to its fair half still
// reads >= 0.9375 of its own limit — above this harness's 0.90 saturation
// gate — so the saturated branch, not the quiet one, is the one evaluated.
func TestSmoke_Decisions_VMCapReachedUnderAggregateLoad(t *testing.T) {
	dep, err := SmokeTarget()
	if err != nil {
		SkipOrFail(t, err)
		return
	}
	indep, err := SmokeIndependentTarget()
	if err != nil {
		SkipOrFail(t, err)
		return
	}
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	// Two preflights and elevations (~3m worst case), build/install, a 4m
	// wait for the cap record and a 1m no-change window.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	for _, target := range []Target{dep, indep} {
		if err := Preflight(ctx, client, target); err != nil {
			t.Fatalf("preflight %s: %v", target.DeviceName, err)
		}
	}
	// Restores each disk's configured resting baseline, not the elevated one.
	defer func() {
		cleanupCtx, cleanupCancel := recoveryContext()
		defer cleanupCancel()
		if err := errors.Join(Cleanup(cleanupCtx, client, dep), Cleanup(cleanupCtx, client, indep)); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	vm, _, err := compute.Discover(ctx, client, dep.VMName)
	if err != nil {
		t.Fatalf("discover VM to read its IOPS pool: %v", err)
	}
	pool := compute.VMIOPSPool(vm)
	elevatedIOPS := pool * 16 / 30 // 3200 on the 6000 pool of a 2-vCPU VM
	elevated := map[string]Target{}
	for _, target := range []Target{dep, indep} {
		disk, err := findDisk(vm, target.DeviceName)
		if err != nil {
			t.Fatalf("%v", err)
		}
		target.BaselineIOPS = elevatedIOPS
		if err := ResetDiskToBaseline(ctx, client, target); err != nil {
			t.Fatalf("elevate %s to %d: %v", target.DeviceName, elevatedIOPS, err)
		}
		elevated[disk.DiskName] = target
	}

	disks := SelectedDisksSpec(dep.DeviceName, indep.DeviceName)
	runAgentScenarioWithDisks(t, ctx, dep, project, false, disks, defaultDiscoveryInterval, func(time.Time) error {
		if err := DriveLoad(ctx, dep); err != nil {
			return fmt.Errorf("drive load on %s: %w", dep.DeviceName, err)
		}
		if err := driveLoadToLog(ctx, indep, remoteWorkDir+"/load-sibling.log"); err != nil {
			return fmt.Errorf("drive load on %s: %w", indep.DeviceName, err)
		}

		rec, err := WaitForJournalRecord(ctx, dep, 4*time.Minute, 5*time.Second, isVMPoolCapRecord)
		if err != nil {
			return fmt.Errorf("no vm_cap_reached record with vm_headroom_iops <= current_iops (pool %d): %w", pool, err)
		}
		capped, ok := elevated[rec.Disk]
		if !ok {
			return fmt.Errorf("vm_cap_reached record names disk %q, not one of the two managed smoke disks", rec.Disk)
		}
		t.Logf("%s capped at %d with vm_headroom_iops %d (pool %d)", capped.DeviceName, rec.CurrentIOPS, rec.VMHeadroomIOPS, pool)

		// vm_cap_reached carries target == current; the disk must stay there.
		capped.BaselineIOPS = rec.CurrentIOPS
		return AssertNoIOPSChange(ctx, client, capped, 60*time.Second, 10*time.Second)
	})
}
