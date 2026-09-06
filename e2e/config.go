//go:build e2e

// Package e2e drives live scenarios against MWS VMs and disks.
package e2e

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// Target describes one VM and disk under test and its SSH endpoint.
type Target struct {
	VMName        string
	DeviceName    string
	BaselineIOPS  int64
	ExpectedClass compute.DiskClass

	SSHHost    string
	SSHUser    string
	SSHKeyPath string
}

// requiredEnv reports all missing variables in one error.
func requiredEnv(names ...string) (map[string]string, error) {
	values := make(map[string]string, len(names))
	var missing []string
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			missing = append(missing, n)
			continue
		}
		values[n] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("e2e: missing required environment variables: %v", missing)
	}
	return values, nil
}

// SkipOrFail allows local opt-out but forbids a vacuous pass in CI.
func SkipOrFail(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("e2e: required environment missing on CI: %v", err)
	}
	t.Skipf("e2e: skipping, environment not configured: %v", err)
}

// SmokeTarget builds the persistent PR-tier target from the environment.
func SmokeTarget() (Target, error) {
	env, err := requiredEnv(
		"E2E_VM_NAME",
		"E2E_DEVICE_NAME",
		"E2E_SSH_HOST",
		"E2E_SSH_USER",
		"E2E_SSH_KEY_PATH",
	)
	if err != nil {
		return Target{}, err
	}
	baseline := int64(1000)
	if raw := os.Getenv("E2E_BASELINE_IOPS"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Target{}, fmt.Errorf("e2e: E2E_BASELINE_IOPS: %w", err)
		}
		baseline = v
	}
	return Target{
		VMName:        env["E2E_VM_NAME"],
		DeviceName:    env["E2E_DEVICE_NAME"],
		BaselineIOPS:  baseline,
		ExpectedClass: compute.ClassDependent,
		SSHHost:       env["E2E_SSH_HOST"],
		SSHUser:       env["E2E_SSH_USER"],
		SSHKeyPath:    env["E2E_SSH_KEY_PATH"],
	}, nil
}

// Zone returns the scope forwarded to the remote agent.
func Zone() string {
	return os.Getenv("MWS_ZONE")
}

// Project returns the project used by the production credential chain.
func Project() (string, error) {
	env, err := requiredEnv("MWS_PROJECT")
	if err != nil {
		return "", err
	}
	return env["MWS_PROJECT"], nil
}
