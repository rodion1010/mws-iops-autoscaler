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

// ErrIncarnationMismatch is wrapped into the error returned by either write
// path when the disk re-read immediately before the write carries a
// different metadata.id (IncarnationID) than the Decision being applied was
// computed from. IOPS equality alone cannot catch this: a disk deleted and
// recreated under the same name (an ordinary infra-as-code "replace",
// CLAUDE.md "Disk resource identity") is a genuinely different resource that
// may happen to sit at the same provisioned IOPS as the stale Decision's
// Current, so the pre-write concurrency check would pass and this write
// would silently mutate the wrong disk. Like ErrConcurrentModification this
// is a pre-write refusal — nothing is ever sent to the platform — but it is
// distinct because the remedy differs: the resource the Decision targeted no
// longer exists, so there is nothing to retry against; the next discovery
// cycle onboards the replacement with its own fresh sampling and identity.
var ErrIncarnationMismatch = errors.New("live disk incarnation differs from the decision's expected incarnation — the disk was replaced under the same name")

// incarnationMatches reports whether a live-read incarnation is safe to
// write against given the incarnation the caller expected.
//
//   - expected == "": the Decision was computed without a known incarnation
//     (Discover's own metadata.id read came back empty for this disk) — there
//     is nothing to compare against, so this degrades to the pre-existing
//     IOPS-only precondition, mirroring StorageDisk.IdentityKey's own
//     "degrade gracefully when IncarnationID is unavailable" contract.
//   - expected != "" && live == "": asymmetric with the case above, and
//     deliberately NOT treated the same way. Every real disk this project has
//     access to returns a metadata.id (ADR-018) — an expected value existing
//     at all means Discover successfully read one for this disk. A live read
//     that suddenly comes back empty is not "unavailable," it is unexplained,
//     and could just as easily be a different resource whose read happened
//     to omit the field for some other reason. Fail closed rather than
//     assume it is safe.
//   - expected != "" && live != "": exact match required.
func incarnationMatches(expected, live string) bool {
	if expected == "" {
		return true
	}
	return expected == live
}

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
func UpdateIndependentDiskIOPS(ctx context.Context, c Client, diskName string, expectedCurrent, target int64, expectedIncarnation string, validateOnly bool, idempotencyKey string) (int64, error) {
	live, err := c.GetDisk(ctx, diskName)
	if err != nil {
		return 0, fmt.Errorf("update independent disk %q: re-read before write: %w", diskName, err)
	}
	if live.Etag == "" {
		return 0, fmt.Errorf("update independent disk %q: no etag on re-read — refusing to write without optimistic concurrency", diskName)
	}
	// Before the IOPS precondition: prove this is even the same resource
	// the Decision targeted. A same-name replacement can carry the same
	// SpecIOPS as the stale Current, so the equality check below would pass
	// and mutate the wrong disk (regression: a stale decision must never
	// touch a replacement disk).
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
// validateDependentWriteTarget proves disks — a VM's full attachment list,
// exactly as UpdateDependentDiskIOPS is about to resend it unchanged — is
// safe to write from, and selects the single attachment matching
// deviceName (issue #20). Two invariants the platform should already
// guarantee but this write path must not simply trust:
//
//  1. Every alias in disks is non-empty and unique. Upsert treats a
//     collection element missing from the request as a delete (the
//     comment at this function's call site), so resending an empty or
//     duplicated alias is not a no-op mistake — it is a malformed
//     collection sent to a safety-sensitive write.
//  2. Exactly one attachment carries deviceName. The original
//     implementation kept overwriting targetAlias on every match, so a
//     second attachment sharing deviceName would silently become "the"
//     target — whichever the loop happened to see last, not necessarily
//     the one the caller meant.
//
// Both are checked before any attachment is selected, so an ambiguous
// inventory returns a descriptive error without ever reaching
// UpdateDependentDisk (issue #20's own acceptance criterion).
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
		// target.Alias is already known non-empty from the pass above —
		// re-asserted here, explicitly, as its own named precondition
		// (issue #20's acceptance criteria list it separately from
		// uniqueness) rather than only ever implied by it.
		if target.Alias == "" {
			return "", 0, "", fmt.Errorf("update dependent disk: device_name %q on VM %q has an empty alias", deviceName, vmName)
		}
		// target.DiskName, not target.IncarnationID: a VM read
		// (translateVMResponse, sdk_client.go) never populates
		// StorageDisk.IncarnationID at all (CLAUDE.md:
		// "StorageDiskStatusResponse... does not carry metadata.id") — only
		// Discover's own separate GetDisk call does, for either disk class.
		// The caller performs that same fresh read itself, immediately
		// before the write, using this DiskName.
		return target.Alias, target.IOPS, target.DiskName, nil
	default:
		return "", 0, "", fmt.Errorf("update dependent disk: device_name %q attached %d times on VM %q — refusing an ambiguous target", deviceName, len(matches), vmName)
	}
}

func UpdateDependentDiskIOPS(ctx context.Context, c Client, vmName, deviceName string, expectedCurrent, target int64, expectedIncarnation string, dryRun bool, idempotencyKey string) (int64, error) {
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
	//
	// Before any of that: prove the alias list is actually safe to resend
	// unchanged, and that deviceName picks out exactly one attachment
	// (issue #20). The platform should already guarantee both — but an
	// empty or duplicate alias here would resend a malformed collection,
	// and more than one attachment sharing deviceName would let
	// targetAlias silently pick whichever the loop below saw last,
	// mutating an arbitrary attachment instead of the intended one.
	targetAlias, liveCurrent, targetDiskName, err := validateDependentWriteTarget(vm.Disks, deviceName, vmName)
	if err != nil {
		return 0, err
	}
	aliases := make([]string, len(vm.Disks))
	for i, d := range vm.Disks {
		aliases[i] = d.Alias
	}

	// Prove the attachment is still the resource the Decision targeted,
	// before the IOPS precondition — a same-name replacement can sit at the
	// same IOPS (regression: a stale decision must never touch a
	// replacement disk). Unlike the independent path, the VM read just
	// above cannot answer this on its own: translateVMResponse
	// (sdk_client.go) never populates a live StorageDisk's IncarnationID at
	// all — the VM's own per-attachment view carries no metadata.id
	// (CLAUDE.md: "StorageDiskStatusResponse... does not carry
	// metadata.id"); only a separate GetDisk call does, uniformly for
	// either disk class (Discover already makes this exact call per
	// managed disk, discovery.go). So proving incarnation here needs its
	// own fresh read — skipped entirely when expectedIncarnation is empty
	// (nothing to compare against, and no reason to pay for a read the
	// result of which incarnationMatches would ignore anyway).
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
