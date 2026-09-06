//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

// TestFull_ProvisionScaleUpTeardown exercises provisioning and the independent
// write path on an ephemeral merge-tier target.
func TestFull_ProvisionScaleUpTeardown(t *testing.T) {
	if _, err := requiredEnv(fullEnv...); err != nil {
		SkipOrFail(t, err)
		return
	}
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Run-scoped names let teardown recover from partial provisioning.
	defer func() {
		teardownCtx, teardownCancel := recoveryContext()
		defer teardownCancel()
		if err := runScript(teardownCtx, "teardown.sh"); err != nil {
			t.Errorf("teardown: %v", err)
		}
	}()

	vmName, deviceName, sshHost, err := provision(ctx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	target := Target{
		VMName:        vmName,
		DeviceName:    deviceName,
		BaselineIOPS:  1000,
		ExpectedClass: compute.ClassIndependent,
		SSHHost:       sshHost, // the VM's own external IP — a bare VM name is not SSH-resolvable
		SSHUser:       os.Getenv("E2E_SSH_USER"),
		SSHKeyPath:    os.Getenv("E2E_SSH_KEY_PATH"),
	}

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	if err := AssertDiskClass(ctx, client, target); err != nil {
		t.Fatalf("verify disk class: %v", err)
	}

	// A fresh VM needs only the remote directory, not shared-target recovery.
	if _, err := target.Run(ctx, "mkdir -p "+remoteWorkDir); err != nil {
		t.Fatalf("prepare remote work dir: %v", err)
	}

	binPath, err := BuildAgentBinary(ctx, "..")
	if err != nil {
		t.Fatalf("build agent binary: %v", err)
	}
	defer os.Remove(binPath)

	if err := InstallAgent(ctx, target, binPath); err != nil {
		t.Fatalf("install agent: %v", err)
	}
	if err := StartAgent(ctx, target, project); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	agentStarted := time.Now()
	defer func() {
		stopCtx, stopCancel := recoveryContext()
		defer stopCancel()
		if err := StopAgent(stopCtx, target); err != nil {
			t.Logf("stop agent: %v", err)
		}
	}()

	if err := DriveLoad(ctx, target); err != nil {
		t.Fatalf("drive load: %v", err)
	}

	final, err := WaitForScaleUp(ctx, client, target, 3*time.Minute, agentStarted)
	if err != nil {
		diagnosticCtx, diagnosticCancel := recoveryContext()
		journal := FetchJournal(diagnosticCtx, target)
		diagnosticCancel()
		t.Fatalf("%v\njournal:\n%s", err, journal)
	}
	t.Logf("scaled from baseline %d to %d", target.BaselineIOPS, final)
}

// provision returns the ready target reported by provision.sh.
func provision(ctx context.Context) (vmName, deviceName, sshHost string, err error) {
	out, err := exec.CommandContext(ctx, "bash", "scripts/provision.sh").CombinedOutput()
	if err != nil {
		return "", "", "", fmt.Errorf("provision.sh: %w: %s", err, out)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "vm_name="):
			vmName = strings.TrimPrefix(line, "vm_name=")
		case strings.HasPrefix(line, "device_name="):
			deviceName = strings.TrimPrefix(line, "device_name=")
		case strings.HasPrefix(line, "ssh_host="):
			sshHost = strings.TrimPrefix(line, "ssh_host=")
		}
	}
	if vmName == "" || deviceName == "" || sshHost == "" {
		return "", "", "", fmt.Errorf("provision.sh: did not report vm_name/device_name/ssh_host, output:\n%s", out)
	}
	return vmName, deviceName, sshHost, nil
}

func runScript(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "bash", "scripts/"+name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}
