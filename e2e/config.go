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
	// DeviceName must be the exact value VM status reports — the same
	// value compute.StorageDisk.GuestPath() and MetricLabel() expect
	// verbatim. The repository variables this is sourced from
	// (E2E_DEVICE_NAME, E2E_SMOKE_INDEPENDENT_DEVICE_NAME) must hold that
	// value directly.
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

// parseBaselineIOPS reads an optional baseline-IOPS override from envVar,
// defaulting to 1000 — this harness's convention for a freshly reset disk —
// when unset.
func parseBaselineIOPS(envVar string) (int64, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return 1000, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("e2e: %s: %w", envVar, err)
	}
	return v, nil
}

// SmokeTarget builds the persistent PR-tier target for the dependent disk
// from the environment.
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
	baseline, err := parseBaselineIOPS("E2E_BASELINE_IOPS")
	if err != nil {
		return Target{}, err
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

// SmokeIndependentTarget builds the persistent PR-tier target for the
// independent disk permanently attached to the same VM as SmokeTarget — it
// is what lets the PR tier exercise the independent write path instead of
// only the full tier's ephemeral disk. SSH access is shared with SmokeTarget
// since both point at the same VM; only the disk differs.
func SmokeIndependentTarget() (Target, error) {
	env, err := requiredEnv(
		"E2E_VM_NAME",
		"E2E_SMOKE_INDEPENDENT_DEVICE_NAME",
		"E2E_SSH_HOST",
		"E2E_SSH_USER",
		"E2E_SSH_KEY_PATH",
	)
	if err != nil {
		return Target{}, err
	}
	baseline, err := parseBaselineIOPS("E2E_SMOKE_INDEPENDENT_BASELINE_IOPS")
	if err != nil {
		return Target{}, err
	}
	return Target{
		VMName:        env["E2E_VM_NAME"],
		DeviceName:    env["E2E_SMOKE_INDEPENDENT_DEVICE_NAME"],
		BaselineIOPS:  baseline,
		ExpectedClass: compute.ClassIndependent,
		SSHHost:       env["E2E_SSH_HOST"],
		SSHUser:       env["E2E_SSH_USER"],
		SSHKeyPath:    env["E2E_SSH_KEY_PATH"],
	}, nil
}

// Project returns the project used by the production credential chain.
func Project() (string, error) {
	env, err := requiredEnv("MWS_PROJECT")
	if err != nil {
		return "", err
	}
	return env["MWS_PROJECT"], nil
}
