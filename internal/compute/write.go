package compute

import (
	"context"
	"errors"
	"fmt"
)

// ErrConcurrentModification marks a pre-write refusal when live IOPS differs from
// the value used to compute the decision. Callers must rediscover instead of retrying
// the stale target.
var ErrConcurrentModification = errors.New("live current differs from the decision's expected current — concurrent modification")

// ErrIncarnationMismatch marks a pre-write refusal when live identity differs from
// the decision or cannot be verified. Callers must rediscover and must not retry the stale target.
var ErrIncarnationMismatch = errors.New("live disk incarnation differs from the decision's expected incarnation — the disk was replaced under the same name")

// incarnationMatches fails closed when an expected identity cannot be verified.
// An empty expected ID degrades to IOPS-only concurrency; otherwise the live ID must
// be present and equal.
func incarnationMatches(expected, live string) bool {
	if expected == "" {
		return true
	}
	return expected == live
}

// UpdateIndependentDiskIOPS conditionally updates a standalone disk's IOPS.
// It re-reads identity and current IOPS, writes only spec.iops with the fresh etag,
// and verifies the applied value. validateOnly uses the platform's server-side validation.
func UpdateIndependentDiskIOPS(ctx context.Context, c Client, diskName string, expectedCurrent, target int64, expectedIncarnation string, validateOnly bool, idempotencyKey string) (int64, error) {
	live, err := c.GetDisk(ctx, diskName)
	if err != nil {
		return 0, fmt.Errorf("update independent disk %q: re-read before write: %w", diskName, err)
	}
	if live.Etag == "" {
		return 0, fmt.Errorf("update independent disk %q: no etag on re-read — refusing to write without optimistic concurrency", diskName)
	}
	// Check identity before IOPS because a replacement may have the same value.
	if !incarnationMatches(expectedIncarnation, live.IncarnationID) {
		return 0, fmt.Errorf("update independent disk %q: %w (expected incarnation %q, live %q)",
			diskName, ErrIncarnationMismatch, expectedIncarnation, live.IncarnationID)
	}
	if live.SpecIOPS != expectedCurrent {
		return 0, fmt.Errorf("update independent disk %q: %w (expected current %d, live %d)",
			diskName, ErrConcurrentModification, expectedCurrent, live.SpecIOPS)
	}
	if !directionPreserved(expectedCurrent, target, live.SpecIOPS) {
		return 0, fmt.Errorf("update independent disk %q: %w — target %d would cross live current %d in the wrong direction",
			diskName, ErrConcurrentModification, target, live.SpecIOPS)
	}

	disk, err := c.UpdateIndependentDisk(ctx, IndependentUpdateRequest{
		DiskName:       diskName,
		IOPS:           target,
		Etag:           live.Etag,
		ValidateOnly:   validateOnly,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return 0, fmt.Errorf("update independent disk %q: %w", diskName, err)
	}
	if validateOnly {
		return target, nil
	}
	if disk.SpecIOPS != target {
		return 0, fmt.Errorf("update independent disk %q: applied %d, wanted %d — treating as failure, not success",
			diskName, disk.SpecIOPS, target)
	}
	return disk.SpecIOPS, nil
}

// validateDependentWriteTarget verifies that attachment aliases can be safely resent
// and that deviceName identifies exactly one dependent disk.
func validateDependentWriteTarget(disks []StorageDisk, deviceName, vmName string) (targetAlias string, liveCurrent int64, targetDiskName string, err error) {
	aliasCount := make(map[string]int, len(disks))
	for _, d := range disks {
		if d.Alias == "" {
			return "", 0, "", fmt.Errorf("update dependent disk: VM %q has an attachment (device_name %q) with an empty alias — refusing to write an ambiguous collection", vmName, d.DeviceName)
		}
		aliasCount[d.Alias]++
	}
	for alias, count := range aliasCount {
		if count > 1 {
			return "", 0, "", fmt.Errorf("update dependent disk: VM %q has %d attachments sharing alias %q — refusing to write an ambiguous collection", vmName, count, alias)
		}
	}

	var matches []StorageDisk
	for _, d := range disks {
		if d.DeviceName == deviceName {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return "", 0, "", fmt.Errorf("update dependent disk: device_name %q not found on VM %q", deviceName, vmName)
	case 1:
		target := matches[0]
		if target.Class != ClassDependent {
			return "", 0, "", fmt.Errorf("update dependent disk: device_name %q on VM %q is independent, not dependent", deviceName, vmName)
		}
		if target.Alias == "" {
			return "", 0, "", fmt.Errorf("update dependent disk: device_name %q on VM %q has an empty alias", deviceName, vmName)
		}
		// VM attachments omit incarnation IDs, so the caller verifies identity via DiskName.
		return target.Alias, target.IOPS, target.DiskName, nil
	default:
		return "", 0, "", fmt.Errorf("update dependent disk: device_name %q attached %d times on VM %q — refusing an ambiguous target", deviceName, len(matches), vmName)
	}
}

// UpdateDependentDiskIOPS conditionally updates a VM-owned disk from a fresh VM snapshot.
// Dry-run performs local preconditions only because VM updates lack server-side validation.
func UpdateDependentDiskIOPS(ctx context.Context, c Client, vmName, deviceName string, expectedCurrent, target int64, expectedIncarnation string, dryRun bool, idempotencyKey string) (int64, error) {
	vm, err := c.GetVirtualMachine(ctx, vmName)
	if err != nil {
		return 0, fmt.Errorf("update dependent disk: re-read VM %q: %w", vmName, err)
	}
	if vm.Etag == "" {
		return 0, fmt.Errorf("update dependent disk: VM %q has no etag — refusing to write without optimistic concurrency", vmName)
	}

	// Upsert treats omitted attachments as deletes, so resend the complete validated alias list.
	// Only the selected alias receives a new IOPS value.
	targetAlias, liveCurrent, targetDiskName, err := validateDependentWriteTarget(vm.Disks, deviceName, vmName)
	if err != nil {
		return 0, err
	}
	aliases := make([]string, len(vm.Disks))
	for i, d := range vm.Disks {
		aliases[i] = d.Alias
	}

	// VM attachments do not expose disk identity; verify it with a fresh disk read.
	// Skip the extra read only when the decision has no expected incarnation.
	if expectedIncarnation != "" {
		liveDisk, err := c.GetDisk(ctx, targetDiskName)
		if err != nil {
			return 0, fmt.Errorf("update dependent disk %q on VM %q: re-read disk %q for incarnation check: %w",
				deviceName, vmName, targetDiskName, err)
		}
		if !incarnationMatches(expectedIncarnation, liveDisk.IncarnationID) {
			return 0, fmt.Errorf("update dependent disk %q on VM %q: %w (expected incarnation %q, live %q)",
				deviceName, vmName, ErrIncarnationMismatch, expectedIncarnation, liveDisk.IncarnationID)
		}
	}
	if liveCurrent != expectedCurrent {
		return 0, fmt.Errorf("update dependent disk %q on VM %q: %w (expected current %d, live %d)",
			deviceName, vmName, ErrConcurrentModification, expectedCurrent, liveCurrent)
	}
	if !directionPreserved(expectedCurrent, target, liveCurrent) {
		return 0, fmt.Errorf("update dependent disk %q on VM %q: %w — target %d would cross live current %d in the wrong direction",
			deviceName, vmName, ErrConcurrentModification, target, liveCurrent)
	}

	if dryRun {
		return target, nil
	}

	updated, err := c.UpdateDependentDisk(ctx, DependentUpdateRequest{
		VMName:         vmName,
		Etag:           vm.Etag,
		Aliases:        aliases,
		TargetAlias:    targetAlias,
		TargetIOPS:     target,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return 0, fmt.Errorf("update dependent disk %q on VM %q: %w", deviceName, vmName, err)
	}

	for _, d := range updated.Disks {
		if d.DeviceName != deviceName {
			continue
		}
		if d.IOPS != target {
			return 0, fmt.Errorf("update dependent disk %q on VM %q: applied %d, wanted %d — treating as failure, not success",
				deviceName, vmName, d.IOPS, target)
		}
		return d.IOPS, nil
	}
	return 0, fmt.Errorf("update dependent disk %q on VM %q: missing from the update response", deviceName, vmName)
}

// directionPreserved independently prevents an update from crossing against its intended direction.
// It remains useful if strict current-IOPS equality is later relaxed.
func directionPreserved(expectedCurrent, target, live int64) bool {
	switch {
	case target > expectedCurrent:
		return target >= live
	case target < expectedCurrent:
		return target <= live
	default:
		return true
	}
}
