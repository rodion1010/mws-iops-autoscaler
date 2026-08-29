package compute

import (
	"context"
	"fmt"
)

// UpdateIndependentDiskIOPS writes a new IOPS limit on a standalone disk
// (US-011). Only spec.iops is sent; the SDK's UpdateDisk already targets
// ?updateOnly=true, so this can never accidentally create a disk.
//
// validateOnly runs the platform's real validation (bounds, the 100-IOPS
// step, permissions) without mutating (US-013) — this is available here
// because UpdateDisk supports it. UpdateDependentDiskIOPS below has no such
// option: the VM update endpoint does not accept validateOnly at all.
func UpdateIndependentDiskIOPS(ctx context.Context, c Client, diskName string, iops int64, validateOnly bool, idempotencyKey string) (int64, error) {
	disk, err := c.UpdateIndependentDisk(ctx, IndependentUpdateRequest{
		DiskName:       diskName,
		IOPS:           iops,
		ValidateOnly:   validateOnly,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return 0, fmt.Errorf("update independent disk %q: %w", diskName, err)
	}
	if validateOnly {
		// A dry run does not mutate — nothing to verify against.
		return iops, nil
	}
	if disk.SpecIOPS != iops {
		return 0, fmt.Errorf("update independent disk %q: applied %d, wanted %d — treating as failure, not success",
			diskName, disk.SpecIOPS, iops)
	}
	return disk.SpecIOPS, nil
}

// UpdateDependentDiskIOPS writes a new IOPS limit on a VM-owned disk
// (US-011a), which the disk API refuses (ADR-003). It re-reads the VM
// itself immediately before writing — never reusing a Discover snapshot
// from the slow path — because the whole point of the "every other field
// byte-identical" check is comparing against state no older than the write
// itself; a stale snapshot could silently clobber a concurrent change.
//
// No dry-run is available for this path: UpdateVirtualMachine has no
// validateOnly field (confirmed against the SDK — see ADR-007). US-013's
// dry-run therefore only ever exercises the independent-disk path for real
// platform validation; a dependent-disk "dry run" can only report intent.
func UpdateDependentDiskIOPS(ctx context.Context, c Client, vmName, deviceName string, iops int64, idempotencyKey string) (int64, error) {
	vm, err := c.GetVirtualMachine(ctx, vmName)
	if err != nil {
		return 0, fmt.Errorf("update dependent disk: re-read VM %q: %w", vmName, err)
	}

	disks := make([]StorageDisk, len(vm.Disks))
	copy(disks, vm.Disks)

	targetIdx := -1
	for i, d := range disks {
		if d.DeviceName != deviceName {
			continue
		}
		if d.Class != ClassDependent {
			return 0, fmt.Errorf("update dependent disk: device_name %q on VM %q is independent, not dependent", deviceName, vmName)
		}
		targetIdx = i
	}
	if targetIdx == -1 {
		return 0, fmt.Errorf("update dependent disk: device_name %q not found on VM %q", deviceName, vmName)
	}
	disks[targetIdx].IOPS = iops

	updated, err := c.UpdateDependentDisk(ctx, DependentUpdateRequest{
		VMName:         vmName,
		Etag:           vm.Etag,
		Disks:          disks,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return 0, fmt.Errorf("update dependent disk %q on VM %q: %w", deviceName, vmName, err)
	}

	for _, d := range updated.Disks {
		if d.DeviceName != deviceName {
			continue
		}
		if d.IOPS != iops {
			return 0, fmt.Errorf("update dependent disk %q on VM %q: applied %d, wanted %d — treating as failure, not success",
				deviceName, vmName, d.IOPS, iops)
		}
		return d.IOPS, nil
	}
	return 0, fmt.Errorf("update dependent disk %q on VM %q: missing from the update response", deviceName, vmName)
}
