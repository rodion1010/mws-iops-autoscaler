//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.mws.cloud/go-sdk/mws"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// NewComputeClient uses the runner's standard credential chain.
func NewComputeClient(ctx context.Context, project string) (compute.Client, error) {
	sdk, err := mws.Load(ctx, mws.WithDefaultProject(project), mws.WithTimeout(30*time.Second))
	if err != nil {
		return nil, fmt.Errorf("e2e: load MWS SDK: %w", err)
	}
	return compute.NewSDKClient(ctx, sdk, project)
}

// findDisk locates a fully discovered disk by its guest device name.
func findDisk(vm *compute.VirtualMachine, deviceName string) (compute.StorageDisk, error) {
	for _, d := range vm.Disks {
		if d.DeviceName == deviceName {
			return d, nil
		}
	}
	return compute.StorageDisk{}, fmt.Errorf("e2e: device %q not found on vm %q", deviceName, vm.Name)
}

// AssertDiskClass verifies the tier's intended write path.
func AssertDiskClass(ctx context.Context, client compute.Client, t Target) error {
	vm, _, err := compute.Discover(ctx, client, t.VMName)
	if err != nil {
		return fmt.Errorf("e2e: discover %q to verify disk class: %w", t.VMName, err)
	}
	disk, err := findDisk(vm, t.DeviceName)
	if err != nil {
		return err
	}
	if disk.Class != t.ExpectedClass {
		return fmt.Errorf("e2e: disk %q has class %q, want %q for this tier", t.DeviceName, disk.Class, t.ExpectedClass)
	}
	return nil
}

// resetTiming is injectable so regression tests can avoid real delays.
type resetTiming struct {
	confirmInterval time.Duration
	confirmDeadline time.Duration
	retryWait       time.Duration
}

// The deadline tolerates the platform's eventually consistent read path.
var defaultResetTiming = resetTiming{
	confirmInterval: 2 * time.Second,
	confirmDeadline: 20 * time.Second,
	retryWait:       3 * time.Second,
}

// ResetDiskToBaseline re-discovers on every attempt and uses the production
// conditional write paths to restore the configured baseline.
func ResetDiskToBaseline(ctx context.Context, client compute.Client, t Target) error {
	return resetDiskToBaseline(ctx, client, t, defaultResetTiming)
}

func resetDiskToBaseline(ctx context.Context, client compute.Client, t Target, timing resetTiming) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(timing.retryWait):
			}
		}
		if err := resetDiskToBaselineOnce(ctx, client, t, timing); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("e2e: reset disk to baseline: giving up after %d attempts: %w", maxAttempts, lastErr)
}

func resetDiskToBaselineOnce(ctx context.Context, client compute.Client, t Target, timing resetTiming) error {
	vm, _, err := compute.Discover(ctx, client, t.VMName)
	if err != nil {
		return fmt.Errorf("e2e: discover %q: %w", t.VMName, err)
	}
	disk, err := findDisk(vm, t.DeviceName)
	if err != nil {
		return err
	}
	if disk.IOPS != t.BaselineIOPS {
		// The platform requires a UUID-formatted idempotency key.
		idempotencyKey := uuid.NewString()
		switch disk.Class {
		case compute.ClassIndependent:
			if _, err := compute.UpdateIndependentDiskIOPS(ctx, client, disk.DiskName, disk.IOPS, t.BaselineIOPS, disk.IncarnationID, false, idempotencyKey); err != nil {
				return fmt.Errorf("e2e: reset independent disk %q to baseline %d: %w", t.DeviceName, t.BaselineIOPS, err)
			}
		case compute.ClassDependent:
			if _, err := compute.UpdateDependentDiskIOPS(ctx, client, t.VMName, t.DeviceName, disk.IOPS, t.BaselineIOPS, disk.IncarnationID, false, idempotencyKey); err != nil {
				return fmt.Errorf("e2e: reset dependent disk %q to baseline %d: %w", t.DeviceName, t.BaselineIOPS, err)
			}
		default:
			return fmt.Errorf("e2e: disk %q has unrecognized class %q", t.DeviceName, disk.Class)
		}
	}
	// Confirm through fresh reads because the read path is eventually consistent.
	return confirmBaseline(ctx, client, t, timing)
}

// confirmBaseline waits for consecutive fresh observations and fails closed
// if the read path does not converge before the deadline.
func confirmBaseline(ctx context.Context, client compute.Client, t Target, timing resetTiming) error {
	const consecutiveBaselineReads = 2
	deadline := time.Now().Add(timing.confirmDeadline)
	streak := 0
	for {
		current, err := CurrentIOPS(ctx, client, t)
		if err != nil {
			return fmt.Errorf("e2e: confirm baseline %d for %q on %q: %w", t.BaselineIOPS, t.DeviceName, t.VMName, err)
		}
		if current == t.BaselineIOPS {
			streak++
			if streak >= consecutiveBaselineReads {
				return nil
			}
		} else {
			streak = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e: confirm baseline %d for %q on %q: still reading %d after %s — stale reads or a racing writer (an orphaned agent from a cancelled run?); refusing to proceed",
				t.BaselineIOPS, t.DeviceName, t.VMName, current, timing.confirmDeadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(timing.confirmInterval):
		}
	}
}

// CurrentIOPS reads provisioned IOPS from the compute API.
func CurrentIOPS(ctx context.Context, client compute.Client, t Target) (int64, error) {
	vm, _, err := compute.Discover(ctx, client, t.VMName)
	if err != nil {
		return 0, fmt.Errorf("e2e: discover %q: %w", t.VMName, err)
	}
	disk, err := findDisk(vm, t.DeviceName)
	if err != nil {
		return 0, err
	}
	return disk.IOPS, nil
}
