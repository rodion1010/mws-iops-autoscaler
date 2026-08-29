package compute

import (
	"context"
	"fmt"
	"net/http"

	"go.mws.cloud/go-sdk/pkg/imds"
	"go.mws.cloud/util-toolset/pkg/os/env"
)

// instanceNameKey is the metadata key for the running VM's own name.
// Unverified against a live MWS VM in this session (no credentials
// available) — but not a guess: it is the exact key
// go.mws.cloud/go-sdk/mws/credentials.OnComputeVM uses internally
// (mws/credentials/vm_service_account.go, nameKey = "instance/name") to
// detect whether the process is running on a Compute VM at all, so the SDK
// itself already depends on this key resolving correctly. Confirm against
// docs/CLAUDE.md's test environment before trusting this in local mode.
const instanceNameKey = "instance/name"

// SelfVMName resolves the running VM's own name from the instance metadata
// service (US-002: "Agent resolves its own VM identity from the metadata
// service"). local mode only — there is no metadata service to ask in
// fleet mode, where VMs are named in config instead (US-003).
func SelfVMName(ctx context.Context) (string, error) {
	client := imds.NewClient(http.DefaultClient, env.RealEnv{})
	name, err := client.GetWithContext(ctx, instanceNameKey)
	if err != nil {
		return "", fmt.Errorf("resolve own VM name from the metadata service: %w", err)
	}
	if name == "" {
		return "", fmt.Errorf("metadata service returned an empty VM name for %q", instanceNameKey)
	}
	return name, nil
}
