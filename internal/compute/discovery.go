package compute

import (
	"context"
	"errors"
	"fmt"
)

// Skip reason categories (issue #4) — stable, low-cardinality codes for
// telemetry labels. Reason (below) carries the free-text detail (disk
// names, VM names, the underlying GetDisk error) for logs; Category is
// what's safe to put on a Prometheus label, since Reason's text is
// unbounded and would blow up label cardinality if used directly.
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

// SkippedDisk records a disk entry Discover could not resolve as safely
// manageable — an ambiguous or missing class, an empty or duplicate
// identity, a failed GetDisk read, or an invalid current IOPS value. It is
// excluded from the returned VirtualMachine.Disks entirely (US-002:
// "skipped with a warning, not a crash" means excluded from the managed
// set, not kept with a IOPS==0 that would misrepresent it as pinned at the
// platform minimum). Discover itself does not log; the caller decides how
// to surface these.
type SkippedDisk struct {
	Alias    string
	DiskName string
	Category string
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

	// Duplicate-identity detection (issue #4): a second attachment sharing
	// either the cloud resource name (DiskName, the GetDisk/UpdateDisk
	// target) or the device_name (DeviceName, registry.Select's join key
	// per ADR-004) makes both entries ambiguous — registry.Select's
	// by-device-name map would otherwise silently let the second one win,
	// with no diagnostic that the first was ever dropped. Neither should
	// ever happen on a real VM; treated as an eligibility failure, not
	// guessed at, if it does. Empty DeviceName is common and expected
	// (platform hasn't assigned one yet) and is deliberately excluded from
	// this check — it's not an identity collision, and local mode's guest-
	// path verification (agent.go) independently excludes an unresolvable
	// empty-device disk on its own.
	// Alias uniqueness (issue #20): unlike DeviceName, an empty Alias is
	// never expected — every attachment is created with one — so it is
	// checked unconditionally, not only when non-empty. Aliases are the
	// dependent write path's own join key (write.go's
	// validateDependentWriteTarget); a disk whose alias collides with
	// another's is unsafe to offer as a management target even before any
	// write is attempted, since UpdateDependentDiskIOPS would refuse it
	// anyway on its own fresh read — better to never select it in the
	// first place than to discover the write path always aborts.
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
			// Excluded from the managed set, not fatal to discovery as a
			// whole (US-002) — but excluded, not left in with IOPS==0.
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
		// metadata.id, from the same GetDisk call — see StorageDisk.
		// IncarnationID's own comment. Left empty (degrading
		// IdentityKey() to DiskName alone) rather than excluding the
		// disk if the response carried none: unlike SpecIOPS<=0, an
		// absent incarnation id isn't evidence of a broken disk, just a
		// weaker identity guarantee for this one cycle.
		sd.IncarnationID = disk.IncarnationID
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
