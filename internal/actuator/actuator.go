// Package actuator applies a decider.Decision through the correct Compute
// API write path (design.md §3.2/§4.7, ADR-003) — the only component in
// the pipeline that mutates anything. Everything upstream (Analyzer,
// Decider) is pure; this is where that stops.
package actuator

import (
	"context"
	"fmt"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
)

// Target identifies where a Decision's Target IOPS actually gets written —
// routing information Decision itself doesn't carry, deliberately: Decide
// only needs policy/state/evidence to pick a number (design.md §3.3), not
// which VM a disk lives on or how it's attached. The caller (the
// not-yet-built orchestrator) already has this from the same Discover call
// that produced current/vm.
type Target struct {
	VMName     string
	DeviceName string // unprefixed (ADR-004) — dependent-path target
	DiskName   string // cloud resource name — independent-path target (GetDisk/UpdateDisk)
	Class      compute.DiskClass
}

// Applied is the value actually verified from the write's response, never
// just what was requested (design.md §4.7: "A write whose response does
// not carry the requested value is a failure, not a success").
type Applied struct {
	DiskID string
	IOPS   int64
}

// Actuator is implemented by ComputeActuator; an interface so callers can
// substitute a fake, matching every other pipeline stage in this codebase.
type Actuator interface {
	// dryRun only has a server-confirmed effect on Target.Class ==
	// ClassIndependent (UpdateDisk's ValidateOnly, US-013). On
	// ClassDependent it reports intent without calling the write endpoint
	// at all — UpdateVirtualMachine has no validateOnly (ADR-007), so
	// there is no server-side dry run to ask for on that path.
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
		return Applied{}, fmt.Errorf("actuator: refusing to apply action %q for disk %q — only scale_up/scale_down write anything",
			d.Action, d.DiskID)
	}

	switch target.Class {
	case compute.ClassIndependent:
		applied, err := compute.UpdateIndependentDiskIOPS(ctx, a.Client, target.DiskName, d.Target, dryRun, idempotencyKey)
		if err != nil {
			return Applied{}, err
		}
		return Applied{DiskID: d.DiskID, IOPS: applied}, nil

	case compute.ClassDependent:
		if dryRun {
			return Applied{DiskID: d.DiskID, IOPS: d.Target}, nil
		}
		applied, err := compute.UpdateDependentDiskIOPS(ctx, a.Client, target.VMName, target.DeviceName, d.Target, idempotencyKey)
		if err != nil {
			return Applied{}, err
		}
		return Applied{DiskID: d.DiskID, IOPS: applied}, nil

	default:
		return Applied{}, fmt.Errorf("actuator: disk %q has unknown class %q", d.DiskID, target.Class)
	}
}
