//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
)

// buildCommand targets the guest architecture independently of the runner.
func buildCommand(ctx context.Context, moduleDir, outPath string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", outPath, "./cmd/mws-iops-autoscaler")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	return cmd
}
