// Package registry is design.md §3.2's "Disk Registry": it applies
// disks.mode (docs/configuration.md §4.2) against a VM's discovered disks
// and resolves each selected disk's effective policy — "apply selection
// strategy → managed set / resolve EffectivePolicy per disk" (design.md
// §4.1's discovery flow). Rebuilt each discovery cycle; it holds no state
// of its own between calls.
package registry

import (
	"fmt"
	"strings"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
)

// ManagedDisk pairs one discovered disk with the resolved policy that
// applies to it.
type ManagedDisk struct {
	Disk   compute.StorageDisk
	Policy config.ResolvedPolicy
}

// Select applies spec against vm's discovered disks, matching by
// unprefixed device_name (docs/design.md §4.1), and resolves each one's policy from
// resolved.DiskPolicies, falling back to resolved.VMPolicy — the
// inheritance chain docs/configuration.md §5 describes, already computed
// by config.Resolve; this function only decides which disks it applies to.
//
// missing names a device_name listed in disks.selected that wasn't found
// among vm's disks. For auto-discovery's exclude list there is no such
// concept — an exclusion naming a disk that doesn't exist is simply a
// no-op, the same "skipped with a warning, not a crash" principle
// US-002/compute.Discover use for a disk GetDisk can't resolve. For
// disks.selected it is different (issue #5, reopened): the list is an
// explicit, exhaustive statement of operator intent, not a best-effort
// filter — a config/discovery mismatch there means the agent's actual
// managed set would silently diverge from what the operator asked for, so
// Select returns it as part of a non-nil err instead of continuing with a
// partial set. The caller (Agent.discover) must not publish that partial
// set or mark discovery successful on this error — it already doesn't,
// since err short-circuits before a.managed/a.sampler.SetDisks are ever
// touched.
func Select(vm *compute.VirtualMachine, spec config.DisksSpec, resolved *config.ResolvedConfig) (managed []ManagedDisk, missing []string, err error) {
	switch spec.Mode {
	case "auto-discovery":
		exclude := make(map[string]bool, len(spec.Exclude))
		for _, name := range spec.Exclude {
			exclude[name] = true
		}
		for _, d := range vm.Disks {
			if exclude[d.DeviceName] {
				continue
			}
			managed = append(managed, ManagedDisk{Disk: d, Policy: policyFor(d.DeviceName, resolved)})
		}
		return managed, nil, nil

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
				missing = append(missing, sel.DeviceName)
				continue
			}
			managed = append(managed, ManagedDisk{Disk: d, Policy: policyFor(sel.DeviceName, resolved)})
		}
		if len(missing) > 0 {
			return nil, missing, fmt.Errorf("disks.selected: device_name(s) not found on VM %q: %s", vm.Name, strings.Join(missing, ", "))
		}
		return managed, nil, nil

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
