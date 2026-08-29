package compute

import (
	"context"
	"fmt"
)

// SkippedDisk records a disk entry Discover could not resolve — its GetDisk
// call failed, so its IOPS/status are unknown. It is excluded from the
// returned VirtualMachine.Disks entirely (US-002: "skipped with a warning,
// not a crash" means excluded from the managed set, not kept with a IOPS==0
// that would misrepresent it as pinned at the platform minimum). Discover
// itself does not log; the caller decides how to surface these.
type SkippedDisk struct {
	Alias    string
	DiskName string
	Reason   error
}

// Discover is the slow-path read described in design.md §4.1: fetch the VM,
// classify each disk (ADR-003), resolve the VM's vCPU count via its VmType,
// and read each disk's own spec/status. It is idempotent and rebuilds the
// whole picture on every call — a disk that changed class, was detached, or
// was resized externally is picked up here, not cached across calls.
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

	resolved := make([]StorageDisk, 0, len(vm.Disks))
	var skipped []SkippedDisk
	for _, sd := range vm.Disks {
		disk, err := c.GetDisk(ctx, sd.DiskName)
		if err != nil {
			// Excluded from the managed set, not fatal to discovery as a
			// whole (US-002) — but excluded, not left in with IOPS==0.
			skipped = append(skipped, SkippedDisk{Alias: sd.Alias, DiskName: sd.DiskName, Reason: err})
			continue
		}
		sd.IOPS = disk.SpecIOPS
		sd.BaseIOPS = disk.BaseIOPS
		sd.ExtraIOPS = disk.ExtraIOPS
		sd.TotalIOPS = disk.TotalIOPS
		resolved = append(resolved, sd)
	}
	vm.Disks = resolved

	return vm, skipped, nil
}

// VMIOPSPool is the VM's aggregate IOPS ceiling — 3000 × vCPU, not enforced
// by the platform (CLAUDE.md "IOPS limits"; the check is entirely the
// agent's responsibility).
const iopsPerVCPU = 3000

func VMIOPSPool(vm *VirtualMachine) int64 {
	return int64(vm.VCPUCount) * iopsPerVCPU
}
