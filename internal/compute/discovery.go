package compute

import (
	"context"
	"errors"
	"fmt"
)

// Skip categories are stable, low-cardinality telemetry labels; Reason holds diagnostic text.
const (
	SkipReasonAmbiguousClass    = "ambiguous_class"
	SkipReasonEmptyIdentity     = "empty_identity"
	SkipReasonDuplicateDiskName = "duplicate_disk_name"
	SkipReasonDuplicateDevice   = "duplicate_device_name"
	SkipReasonGetDiskFailed     = "get_disk_failed"
	SkipReasonInvalidIOPS       = "invalid_iops"
	SkipReasonEmptyAlias        = "empty_alias"
	SkipReasonDuplicateAlias    = "duplicate_alias"
)

// SkippedDisk describes an attachment excluded because it cannot be managed safely.
// Discover leaves logging and telemetry policy to its caller.
type SkippedDisk struct {
	Alias    string
	DiskName string
	Category string
	Reason   error
}

// Discover rebuilds the VM, disk classifications, capacities, and live disk state.
// It does not retain a previous snapshot.
func Discover(ctx context.Context, c Client, vmName string) (*VirtualMachine, []SkippedDisk, error) {
	vm, err := c.GetVirtualMachine(ctx, vmName)
	if err != nil {
		return nil, nil, fmt.Errorf("discover: get VM %q: %w", vmName, err)
	}

	vmType, err := c.GetVmType(ctx, vm.VMType)
	if err != nil {
		return nil, nil, fmt.Errorf("discover: get VM type %q for VM %q: %w", vm.VMType, vmName, err)
	}
	vm.VCPUCount = vmType.VCPUCount

	// Duplicate resource names or device names make attachment selection ambiguous.
	// Empty device names are allowed here but may fail local guest-path eligibility.
	// Aliases must always be non-empty and unique because dependent writes join on them.
	diskNameCount := make(map[string]int, len(vm.Disks))
	deviceNameCount := make(map[string]int, len(vm.Disks))
	aliasCount := make(map[string]int, len(vm.Disks))
	for _, sd := range vm.Disks {
		diskNameCount[sd.DiskName]++
		if sd.DeviceName != "" {
			deviceNameCount[sd.DeviceName]++
		}
		if sd.Alias != "" {
			aliasCount[sd.Alias]++
		}
	}

	resolved := make([]StorageDisk, 0, len(vm.Disks))
	var skipped []SkippedDisk
	for _, sd := range vm.Disks {
		if sd.Class != ClassIndependent && sd.Class != ClassDependent {
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Category: SkipReasonAmbiguousClass,
				Reason: fmt.Errorf("ambiguous or unknown disk class %q — neither disk.ref nor disk.spec resolved", sd.Class)})
			continue
		}
		if sd.DiskName == "" {
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Category: SkipReasonEmptyIdentity,
				Reason: errors.New("empty disk resource identity")})
			continue
		}
		if diskNameCount[sd.DiskName] > 1 {
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Category: SkipReasonDuplicateDiskName,
				Reason: fmt.Errorf("disk resource identity %q attached more than once on VM %q", sd.DiskName, vmName)})
			continue
		}
		if sd.DeviceName != "" && deviceNameCount[sd.DeviceName] > 1 {
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Category: SkipReasonDuplicateDevice,
				Reason: fmt.Errorf("device_name %q attached more than once on VM %q", sd.DeviceName, vmName)})
			continue
		}
		if sd.Alias == "" {
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Category: SkipReasonEmptyAlias,
				Reason: fmt.Errorf("empty alias on VM %q (device_name %q)", vmName, sd.DeviceName)})
			continue
		}
		if aliasCount[sd.Alias] > 1 {
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Category: SkipReasonDuplicateAlias,
				Reason: fmt.Errorf("alias %q attached more than once on VM %q", sd.Alias, vmName)})
			continue
		}

		disk, err := c.GetDisk(ctx, sd.DiskName)
		if err != nil {
			// A single unreadable disk is excluded without failing the VM-wide discovery.
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Category: SkipReasonGetDiskFailed, Reason: err})
			continue
		}
		if disk.SpecIOPS <= 0 {
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Category: SkipReasonInvalidIOPS,
				Reason: fmt.Errorf("invalid current IOPS %d", disk.SpecIOPS)})
			continue
		}
		sd.IOPS = disk.SpecIOPS
		sd.BaseIOPS = disk.BaseIOPS
		sd.ExtraIOPS = disk.ExtraIOPS
		sd.TotalIOPS = disk.TotalIOPS
		// Missing metadata.id weakens identity to name-only but does not make the disk unusable.
		sd.IncarnationID = disk.IncarnationID
		resolved = append(resolved, sd)
	}
	vm.Disks = resolved

	return vm, skipped, nil
}

// VMIOPSPool is the agent-enforced aggregate ceiling of 3000 IOPS per vCPU.
const iopsPerVCPU = 3000

func VMIOPSPool(vm *VirtualMachine) int64 {
	return int64(vm.VCPUCount) * iopsPerVCPU
}
