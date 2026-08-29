package compute

import (
	"context"
	"errors"
	"fmt"
)

// ErrConcurrentModification is wrapped into the error returned by either
// write path (issue #16) when a disk's live current IOPS, re-read
// immediately before the write, no longer matches the value the Decision
// being applied was computed from. A Decision's Target is only meaningful
// relative to the Current it was derived from (§4.5/§4.6's guardrails all
// clamp around it) — if the live value has since moved, that Target may no
// longer even point the right direction, let alone be the right magnitude.
// Callers must treat this as "abort, rediscover, let the next reconcile
// cycle decide again" — never as transient and worth blindly retrying the
// same target.
var ErrConcurrentModification = errors.New("live current differs from the decision's expected current — concurrent modification")

// UpdateIndependentDiskIOPS writes a new IOPS limit on a standalone disk
// (US-011). Only spec.iops (plus metadata.etag) is sent; the SDK's
// UpdateDisk already targets ?updateOnly=true, so this can never
// accidentally create a disk.
//
// expectedCurrent is the live IOPS value the caller's Decision.Target was
// computed relative to (Decision.Current) — this function re-reads the
// disk immediately before writing and refuses to proceed if the two
// disagree (issue #16: a Decision computed from a stale snapshot must
// never overwrite a value that has since changed, in either direction).
// The re-read's own Etag is what's actually sent on the write, never one
// carried from an earlier Discover — the whole point of optimistic
// concurrency is comparing against state no older than the write itself.
//
// validateOnly runs the platform's real validation (bounds, the 100-IOPS
// step, permissions) without mutating (US-013) — this is available here
// because UpdateDisk supports it. It still exercises every precondition
// check above; only the final network write to UpdateDisk carries
// ValidateOnly through, so a dry run reports a would-be conflict exactly
// like a real run would. UpdateDependentDiskIOPS below has no server-side
// validateOnly option: the VM update endpoint does not accept it at all.
func UpdateIndependentDiskIOPS(ctx context.Context, c Client, diskName string, expectedCurrent, target int64, validateOnly bool, idempotencyKey string) (int64, error) {
	live, err := c.GetDisk(ctx, diskName)
	if err != nil {
		return 0, fmt.Errorf("update independent disk %q: re-read before write: %w", diskName, err)
	}
	if live.Etag == "" {
		return 0, fmt.Errorf("update independent disk %q: no etag on re-read — refusing to write without optimistic concurrency", diskName)
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
		// A dry run does not mutate — nothing to verify against.
		return target, nil
	}
	if disk.SpecIOPS != target {
		return 0, fmt.Errorf("update independent disk %q: applied %d, wanted %d — treating as failure, not success",
			diskName, disk.SpecIOPS, target)
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
// expectedCurrent/target/dryRun mirror UpdateIndependentDiskIOPS exactly
// (issue #16), including on the dry-run path: since there is no
// server-side validateOnly for UpdateVirtualMachine (ADR-007), dryRun here
// means "run every precondition check below against a fresh re-read, then
// report intent without calling UpdateDependentDisk at all" — the closest
// analogue this write path has to the independent path's server-confirmed
// dry run.
func UpdateDependentDiskIOPS(ctx context.Context, c Client, vmName, deviceName string, expectedCurrent, target int64, dryRun bool, idempotencyKey string) (int64, error) {
	vm, err := c.GetVirtualMachine(ctx, vmName)
	if err != nil {
		return 0, fmt.Errorf("update dependent disk: re-read VM %q: %w", vmName, err)
	}
	if vm.Etag == "" {
		return 0, fmt.Errorf("update dependent disk: VM %q has no etag — refusing to write without optimistic concurrency", vmName)
	}

	// Every current alias travels in the request, in order — Upsert treats
	// an existing collection element missing from the request as a delete,
	// so dropping one to "only touch the target" would detach every other
	// disk on the VM. Only the target's alias carries a change.
	aliases := make([]string, len(vm.Disks))
	targetAlias := ""
	liveCurrent := int64(0)
	for i, d := range vm.Disks {
		aliases[i] = d.Alias
		if d.DeviceName != deviceName {
			continue
		}
		if d.Class != ClassDependent {
			return 0, fmt.Errorf("update dependent disk: device_name %q on VM %q is independent, not dependent", deviceName, vmName)
		}
		targetAlias = d.Alias
		liveCurrent = d.IOPS
	}
	if targetAlias == "" {
		return 0, fmt.Errorf("update dependent disk: device_name %q not found on VM %q", deviceName, vmName)
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

// directionPreserved re-derives which direction the write is supposed to
// move in from expectedCurrent vs target (the shape the Decider's own
// guardrails already guaranteed when it produced this Decision — target is
// never equal to current for a scale_up/scale_down action) and checks that
// direction still holds against live, the value re-read immediately before
// the write. In the ordinary case this is implied by the equality check
// above (live == expectedCurrent makes the two checks redundant) — it is
// kept as an independent assertion, not folded into the equality check, so
// it keeps holding even if that check is ever loosened to a tolerance band
// instead of strict equality (issue #16's explicit acceptance criterion:
// "a scale-up request can never write a value below the live current;
// future scale-down can never write above it").
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
