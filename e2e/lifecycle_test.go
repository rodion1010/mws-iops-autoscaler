//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// fullEnv lists the provisioning inputs beyond MWS_PROJECT.
var fullEnv = []string{
	"MWS_ZONE", "E2E_RUN_ID", "E2E_SUBNET_ID", "E2E_BOOT_IMAGE",
	"E2E_VM_TYPE", "E2E_AGENT_SA", "E2E_SSH_PUBLIC_KEY",
	"E2E_SSH_USER", "E2E_SSH_KEY_PATH",
}

// fullFixture is the merge tier's shared ephemeral VM, provisioned once with
// three data disks: one with an explicit deviceName (used by
// TestFull_Lifecycle_ProvisionScaleUpTeardown), one with none, so the
// platform defaults its guest device to mws-{alias} — used by
// TestFull_DeviceNaming_NoDeviceNameDefaultsToAlias — and a churn disk
// created but left unattached at creation, for scenarios that must observe
// a live attach or detach (AttachChurnDisk/DetachChurnDisk, e2e/churn_test.go).
// Every full-tier test builds its own Target against one of these rather
// than provisioning again.
//
// The storage-disks entry strings (boot/data/noname) are captured verbatim
// from provision.sh rather than reconstructed here: mws compute vm update
// --storage-disks resends the VM's whole disk list, so
// AttachChurnDisk/DetachChurnDisk must resend the exact entries the VM was
// created with, not a Go-side guess at their syntax.
type fullFixture struct {
	VMName           string
	SSHHost          string
	DataDeviceName   string
	NoNameDeviceName string
	ChurnDeviceName  string

	bootEntry   string
	dataEntry   string
	nonameEntry string
	churnEntry  string
}

func (f fullFixture) dataTarget() Target   { return f.target(f.DataDeviceName) }
func (f fullFixture) noNameTarget() Target { return f.target(f.NoNameDeviceName) }
func (f fullFixture) churnTarget() Target  { return f.target(f.ChurnDeviceName) }

// baseStorageDisks is the VM's known-good list without the churn disk —
// what DetachChurnDisk resends, and what AttachChurnDisk resends plus
// churnEntry.
func (f fullFixture) baseStorageDisks() []string {
	return []string{f.bootEntry, f.dataEntry, f.nonameEntry}
}

func (f fullFixture) target(deviceName string) Target {
	return Target{
		VMName:        f.VMName,
		DeviceName:    deviceName,
		BaselineIOPS:  1000,
		ExpectedClass: compute.ClassIndependent,
		SSHHost:       f.SSHHost, // the VM's own external IP — a bare VM name is not SSH-resolvable
		SSHUser:       os.Getenv("E2E_SSH_USER"),
		SSHKeyPath:    os.Getenv("E2E_SSH_KEY_PATH"),
	}
}

var (
	fullFixtureOnce      sync.Once
	fullFixtureAttempted bool
	fullFixtureVal       fullFixture
	fullFixtureErr       error
)

// ensureFullFixture provisions the merge tier's shared ephemeral VM at most
// once (sync.Once) — so a `-run` selecting only some full-tier tests never
// pays for provisioning it doesn't need, and tests that do run share one VM
// instead of one each; TestMain tears it down, once, after every test has
// run. The env/project checks are cheap and side-effect-free, so they run
// independently for every caller, including ones that arrive after the
// first successful provision — only provisioning itself is guarded.
func ensureFullFixture(t *testing.T) (fullFixture, bool) {
	t.Helper()
	if _, err := requiredEnv(fullEnv...); err != nil {
		SkipOrFail(t, err)
		return fullFixture{}, false
	}
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return fullFixture{}, false
	}

	fullFixtureOnce.Do(func() {
		fullFixtureAttempted = true
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		fullFixtureVal, fullFixtureErr = provisionFullFixture(ctx, project)
	})
	if fullFixtureErr != nil {
		t.Fatalf("provision full-tier fixture: %v", fullFixtureErr)
		return fullFixture{}, false
	}
	return fullFixtureVal, true
}

// fullTarget returns the merge tier's explicitly-named independent disk.
func fullTarget(t *testing.T) Target {
	t.Helper()
	fix, ok := ensureFullFixture(t)
	if !ok {
		return Target{}
	}
	return fix.dataTarget()
}

// fullNoDeviceNameTarget returns the merge tier's independent disk that was
// attached with no deviceName at all.
func fullNoDeviceNameTarget(t *testing.T) Target {
	t.Helper()
	fix, ok := ensureFullFixture(t)
	if !ok {
		return Target{}
	}
	return fix.noNameTarget()
}

// provisionFullFixture provisions the merge tier's ephemeral VM and its disks
// and verifies each one's class, so ensureFullFixture's sync.Once body stays a
// thin wrapper.
func provisionFullFixture(ctx context.Context, project string) (fullFixture, error) {
	fix, err := provision(ctx)
	if err != nil {
		return fullFixture{}, fmt.Errorf("provision: %w", err)
	}
	client, err := NewComputeClient(ctx, project)
	if err != nil {
		return fullFixture{}, fmt.Errorf("build compute client: %w", err)
	}
	for _, target := range []Target{fix.dataTarget(), fix.noNameTarget()} {
		if err := AssertDiskClass(ctx, client, target); err != nil {
			return fullFixture{}, fmt.Errorf("verify disk class (%s): %w", target.DeviceName, err)
		}
	}
	return fix, nil
}

// teardownTimeout sums teardown.sh's bounded phases — the VM delete's
// --wait-timeout 3m, up to 2m waiting for the VM to disappear, then the
// parallel disks' --wait-timeout 2m — plus 1 minute of margin.
const teardownTimeout = 8 * time.Minute

// TestMain tears down the full tier's shared ephemeral VM+disk exactly once,
// after every test in the package has run, rather than per test — fullTarget
// provisions it lazily and shares it across every full-tier test that asks
// for one. Teardown runs whenever provisioning was attempted, even if it
// failed partway: run identity comes from E2E_RUN_ID (set once per workflow
// run, not generated here), which is what lets teardown.sh clean up a
// partial run.
func TestMain(m *testing.M) {
	code := m.Run()
	if fullFixtureAttempted {
		ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
		if err := runScript(ctx, "teardown.sh"); err != nil {
			fmt.Fprintf(os.Stderr, "teardown: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
		cancel()
	}
	os.Exit(code)
}

// TestFull_Lifecycle_ProvisionScaleUpTeardown exercises provisioning and the
// independent write path on an ephemeral merge-tier target.
func TestFull_Lifecycle_ProvisionScaleUpTeardown(t *testing.T) {
	target := fullTarget(t)
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	// fullTarget's VM is shared with other full-tier tests; Preflight resets
	// any state a previous test left behind — the same guarantee the smoke
	// tier relies on for its own shared, persistent target.
	if err := Preflight(ctx, client, target); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	runAgentScenario(t, ctx, target, project, false, func(agentStarted time.Time) error {
		if err := DriveLoad(ctx, target); err != nil {
			return fmt.Errorf("drive load: %w", err)
		}
		final, err := WaitForScaleUp(ctx, client, target, 3*time.Minute, agentStarted)
		if err != nil {
			return err
		}
		t.Logf("scaled from baseline %d to %d", target.BaselineIOPS, final)
		return nil
	})
}

// provision returns the ready fixture reported by provision.sh. Field names
// below match provision.sh's own output keys one-to-one, so a new key added
// there is added here the same way — never reconstructed independently.
func provision(ctx context.Context) (fullFixture, error) {
	out, err := exec.CommandContext(ctx, "bash", "scripts/provision.sh").CombinedOutput()
	if err != nil {
		return fullFixture{}, fmt.Errorf("provision.sh: %w: %s", err, out)
	}
	var fix fullFixture
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "vm_name="):
			fix.VMName = strings.TrimPrefix(line, "vm_name=")
		case strings.HasPrefix(line, "noname_device_name="):
			fix.NoNameDeviceName = strings.TrimPrefix(line, "noname_device_name=")
		case strings.HasPrefix(line, "churn_device_name="):
			fix.ChurnDeviceName = strings.TrimPrefix(line, "churn_device_name=")
		case strings.HasPrefix(line, "device_name="):
			fix.DataDeviceName = strings.TrimPrefix(line, "device_name=")
		case strings.HasPrefix(line, "ssh_host="):
			fix.SSHHost = strings.TrimPrefix(line, "ssh_host=")
		case strings.HasPrefix(line, "storage_disk_boot="):
			fix.bootEntry = strings.TrimPrefix(line, "storage_disk_boot=")
		case strings.HasPrefix(line, "storage_disk_data="):
			fix.dataEntry = strings.TrimPrefix(line, "storage_disk_data=")
		case strings.HasPrefix(line, "storage_disk_noname="):
			fix.nonameEntry = strings.TrimPrefix(line, "storage_disk_noname=")
		case strings.HasPrefix(line, "storage_disk_churn="):
			fix.churnEntry = strings.TrimPrefix(line, "storage_disk_churn=")
		}
	}
	if fix.VMName == "" || fix.DataDeviceName == "" || fix.NoNameDeviceName == "" || fix.ChurnDeviceName == "" || fix.SSHHost == "" ||
		fix.bootEntry == "" || fix.dataEntry == "" || fix.nonameEntry == "" || fix.churnEntry == "" {
		return fullFixture{}, fmt.Errorf("provision.sh: did not report every expected field, output:\n%s", out)
	}
	return fix, nil
}

func runScript(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "bash", "scripts/"+name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}
