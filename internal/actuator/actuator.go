// Package actuator applies a decider.Decision through the correct Compute API
// write path. It is the only pipeline stage that mutates anything.
package actuator

import (
	"context"
	"fmt"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
)

// Target identifies where a Decision's Target IOPS is written. Decision does
// not carry this routing information; the caller has it from discovery.
type Target struct {
	VMName     string
	DeviceName string // the exact value VM status reports — dependent-path target
	DiskName   string // cloud resource name — independent-path target (GetDisk/UpdateDisk)
	Class      compute.DiskClass

	// IncarnationID is the disk's metadata.id the Decision was computed
	// against. The write path checks it before writing, so a disk deleted and
	// recreated under the same name is refused (compute.ErrIncarnationMismatch)
	// rather than mutated. Empty when the platform served no metadata.id; the
	// write path then falls back to its IOPS-only concurrency check.
	IncarnationID string
}

// Applied is the value verified from the write's response, never just what
// was requested: a response without the requested value is a failure.
type Applied struct {
	DiskID string
	IOPS   int64
}

// Actuator is implemented by ComputeActuator; an interface so callers can
// substitute a fake.
type Actuator interface {
	// Apply writes d.Target for scale_up and scale_down only. dryRun is
	// server-confirmed only for ClassIndependent (UpdateDisk's ValidateOnly).
	// On ClassDependent it never calls the write endpoint, because
	// UpdateVirtualMachine has no validateOnly.
	Apply(ctx context.Context, d decider.Decision, target Target, dryRun bool, idempotencyKey string) (Applied, error)
}

// ComputeActuator implements Actuator over a real (or faked)
// compute.Client.
type ComputeActuator struct {
	Client compute.Client
}

func NewComputeActuator(c compute.Client) *ComputeActuator {
	return &ComputeActuator{Client: c}
}

func (a *ComputeActuator) Apply(ctx context.Context, d decider.Decision, target Target, dryRun bool, idempotencyKey string) (Applied, error) {
	if d.Action != decider.ActionScaleUp && d.Action != decider.ActionScaleDown {
		return Applied{}, compute.MarkNoWriteSent(fmt.Errorf("actuator: refusing to apply action %q for disk %q — only scale_up/scale_down write anything",
			d.Action, d.DiskID))
	}

	switch target.Class {
	case compute.ClassIndependent:
		// The write path re-reads the disk and aborts if it no longer equals
		// d.Current, in either direction.
		applied, err := compute.UpdateIndependentDiskIOPS(ctx, a.Client, target.DiskName, d.Current, d.Target, target.IncarnationID, dryRun, idempotencyKey)
		if err != nil {
			return Applied{}, err
		}
		return Applied{DiskID: d.DiskID, IOPS: applied}, nil

	case compute.ClassDependent:
		// dryRun is passed through rather than short-circuited here, so a dry
		// run still exercises the fresh-read, conflict and direction checks.
		applied, err := compute.UpdateDependentDiskIOPS(ctx, a.Client, target.VMName, target.DeviceName, d.Current, d.Target, target.IncarnationID, dryRun, idempotencyKey)
		if err != nil {
			return Applied{}, err
		}
		return Applied{DiskID: d.DiskID, IOPS: applied}, nil

	default:
		return Applied{}, compute.MarkNoWriteSent(fmt.Errorf("actuator: disk %q has unknown class %q", d.DiskID, target.Class))
	}
}
