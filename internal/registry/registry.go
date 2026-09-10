// Package registry is design.md §3.2's "Disk Registry": it applies
// disks.mode (docs/configuration.md §4.2) against a VM's discovered disks
// and resolves each selected disk's effective policy — "apply selection
// strategy → managed set / resolve EffectivePolicy per disk" (design.md
// §4.1's discovery flow). Rebuilt each discovery cycle; it holds no state
// of its own between calls.
package registry

import (
	"fmt"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
)

// ManagedDisk pairs one discovered disk with the resolved policy that
// applies to it.
type ManagedDisk struct {
	Disk   compute.StorageDisk
	Policy config.ResolvedPolicy
}

// Degradation names one disks.selected or disks.exclude entry that could
// not be resolved against vm's live disks this cycle. DeviceName is
// whatever the operator's config named — for ExcludeTargetNotFound it does
// not correspond to any live disk at all. Reason is one of Select's two
// stable category constants, suitable as a Prometheus label.
type Degradation struct {
	DeviceName string
	Reason     string
}

// Select's two Degradation.Reason categories (docs/design.md §4.1,
// docs/configuration.md §4.2).
const (
	ReasonSelectedNotFound      = "selected_not_found"
	ReasonExcludeTargetNotFound = "exclude_target_not_found"
)

// Select applies spec against vm's discovered disks, matching by
// unprefixed device_name (docs/design.md §4.1), and resolves each one's
// policy from resolved.DiskPolicies, falling back to resolved.VMPolicy —
// the inheritance chain docs/configuration.md §5 describes, already
// computed by config.Resolve; this function only decides which disks it
// applies to.
//
// degraded names every disks.selected or disks.exclude entry that could not
// be resolved this cycle — Select always returns the managed set it could
// actually build (never nil on a mismatch) alongside it, because Select has
// no memory of prior cycles (see the package doc) and therefore cannot
// itself decide whether an unresolved name is fatal. That decision needs
// history a single call doesn't have — whether this is the very first
// discovery under the current config (cold start: managing a silently
// partial set would diverge from a operator's stated intent with no prior
// baseline to fall back to) or a later cycle after at least one success
// (runtime: the resolved disks can keep being managed while only the
// unresolved name degrades) — so it belongs to the caller, Agent.discover,
// which does have that history (issue #5, reopened; docs/configuration.md
// §4.2).
//
// err is reserved for a structural problem with spec itself (an unknown
// mode), never for a name that fails to resolve — --validate-config already
// rejects every other mode, so a real config file cannot reach the branch
// that returns it.
func Select(vm *compute.VirtualMachine, spec config.DisksSpec, resolved *config.ResolvedConfig) (managed []ManagedDisk, degraded []Degradation, err error) {
	switch spec.Mode {
	case "auto-discovery":
		exclude := make(map[string]bool, len(spec.Exclude))
		matched := make(map[string]bool, len(spec.Exclude))
		for _, name := range spec.Exclude {
			exclude[name] = true
		}
		for _, d := range vm.Disks {
			if exclude[d.DeviceName] {
				matched[d.DeviceName] = true
				continue
			}
			managed = append(managed, ManagedDisk{Disk: d, Policy: policyFor(d.DeviceName, resolved)})
		}
		// An exclude entry that never matched a live disk is not simply a
		// no-op (there being nothing to exclude never changes what gets
		// managed here) — it may equally be a typo that leaves a disk the
		// operator meant to exclude managed anyway, which the operator has
		// no way to tell apart from the harmless case without a signal.
		for _, name := range spec.Exclude {
			if !matched[name] {
				degraded = append(degraded, Degradation{DeviceName: name, Reason: ReasonExcludeTargetNotFound})
			}
		}
		return managed, degraded, nil

	case "selected":
		// byName is built from vm.Disks, which by the time it reaches here
		// has already had any duplicate device_name attachment excluded
		// entirely by compute.Discover (issue #4) — so this map can never
		// silently let a second live disk overwrite a first one under the
		// same key. That was the other remaining half of this issue's
		// acceptance criteria ("reject ambiguous duplicate live matches");
		// it is closed at the source, not re-checked here.
		byName := make(map[string]compute.StorageDisk, len(vm.Disks))
		for _, d := range vm.Disks {
			byName[d.DeviceName] = d
		}
		for _, sel := range spec.Selected {
			d, ok := byName[sel.DeviceName]
			if !ok {
				degraded = append(degraded, Degradation{DeviceName: sel.DeviceName, Reason: ReasonSelectedNotFound})
				continue
			}
			managed = append(managed, ManagedDisk{Disk: d, Policy: policyFor(sel.DeviceName, resolved)})
		}
		return managed, degraded, nil

	default:
		// --validate-config already rejects any other mode
		// (internal/config/validate.go) — reaching this means the caller
		// skipped validation, not a value a real config file can carry.
		return nil, nil, fmt.Errorf("registry: unknown disks.mode %q", spec.Mode)
	}
}

func policyFor(deviceName string, resolved *config.ResolvedConfig) config.ResolvedPolicy {
	if p, ok := resolved.DiskPolicies[deviceName]; ok {
		return p
	}
	return resolved.VMPolicy
}
