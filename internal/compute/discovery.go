package compute

import (
	"context"
	"fmt"
)

// Discover is the slow-path read described in design.md §4.1: fetch the VM,
// classify each disk (ADR-003), resolve the VM's vCPU count via its VmType,
// and read each disk's own spec/status. It is idempotent and rebuilds the
// whole picture on every call — a disk that changed class, was detached, or
// was resized externally is picked up here, not cached across calls.
func Discover(ctx context.Context, c Client, vmName string) (*VirtualMachine, error) {
	vm, err := c.GetVirtualMachine(ctx, vmName)
	if err != nil {
		return nil, fmt.Errorf("discover: get VM %q: %w", vmName, err)
	}

	vmType, err := c.GetVmType(ctx, vm.VMType)
	if err != nil {
		return nil, fmt.Errorf("discover: get VM type %q for VM %q: %w", vm.VMType, vmName, err)
	}
	vm.VCPUCount = vmType.VCPUCount

	for i, sd := range vm.Disks {
		disk, err := c.GetDisk(ctx, sd.DiskName)
		if err != nil {
			// A disk that fails to resolve is skipped, not fatal to
			// discovery as a whole (US-002: "Disks whose path does not
			// resolve are skipped with a warning, not a crash").
			continue
		}
		vm.Disks[i].IOPS = disk.SpecIOPS
		vm.Disks[i].BaseIOPS = disk.BaseIOPS
		vm.Disks[i].ExtraIOPS = disk.ExtraIOPS
		vm.Disks[i].TotalIOPS = disk.TotalIOPS
	}

	return vm, nil
}

// VMIOPSPool is the VM's aggregate IOPS ceiling — 3000 × vCPU, not enforced
// by the platform (CLAUDE.md "IOPS limits"; the check is entirely the
// agent's responsibility).
const iopsPerVCPU = 3000

func VMIOPSPool(vm *VirtualMachine) int64 {
	return int64(vm.VCPUCount) * iopsPerVCPU
}
