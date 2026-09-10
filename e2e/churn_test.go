//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// setStorageDisks resends vmName's full --storage-disks list via the mws
// CLI. compute.Client (internal/compute/client.go) deliberately exposes no
// attach/detach capability — production code never attaches or detaches a
// disk — so live attach/detach in e2e shells out directly, mirroring
// provision.sh's own use of the CLI.
//
// mws compute vm update --storage-disks resends the whole disk list
// (docs/platform-reference.md, docs/ci-cd.md): entries must be the complete
// known-good set, never just the one being added or removed, or the omitted
// ones are detached. "vm" here, not provision.sh's "virtual-machine" — every
// documented, verified example of --storage-disks on update
// (docs/platform-reference.md, docs/ci-cd.md's runbook) uses the "vm" form
// specifically; provision.sh's "virtual-machine" is only verified for
// create/get/delete, a separate command shape.
func setStorageDisks(ctx context.Context, project, vmName string, entries []string) error {
	args := []string{"compute", "vm", "update",
		fmt.Sprintf("compute/projects/%s/virtualMachines/%s", project, vmName),
	}
	for _, entry := range entries {
		args = append(args, "--storage-disks", entry)
	}
	cmd := exec.CommandContext(ctx, "mws", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("e2e: set storage disks: %w: %s", err, out)
	}
	return nil
}

// AttachChurnDisk attaches fix's churn disk (created by provision.sh but
// left unattached) under its fixed device name, resending the fixture's
// known-good boot/data/noname entries alongside it — see setStorageDisks.
func AttachChurnDisk(ctx context.Context, project string, fix fullFixture) error {
	entries := append(fix.baseStorageDisks(), fix.churnEntry)
	return setStorageDisks(ctx, project, fix.VMName, entries)
}

// DetachChurnDisk resends the fixture's known-good boot/data/noname entries
// without the churn entry, detaching it.
func DetachChurnDisk(ctx context.Context, project string, fix fullFixture) error {
	return setStorageDisks(ctx, project, fix.VMName, fix.baseStorageDisks())
}

// waitForDiskAttached polls until t's device is visible on t's VM, tolerating
// mws compute vm update --storage-disks's own propagation lag — the same lag
// provision.sh's initial creation already retries against before reporting
// the noname disk's deviceName.
func waitForDiskAttached(ctx context.Context, client compute.Client, t Target, timeout, pollInterval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		vm, _, err := compute.Discover(ctx, client, t.VMName)
		if err != nil {
			return fmt.Errorf("e2e: wait for disk attached: %w", err)
		}
		if _, findErr := findDisk(vm, t.DeviceName); findErr == nil {
			return nil
		} else {
			lastErr = findErr
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e: wait for disk attached: %q still not visible on %q after %s: %w", t.DeviceName, t.VMName, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
