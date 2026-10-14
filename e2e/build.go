//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// buildCommand targets the guest architecture independently of the runner.
// tags, when non-empty, is passed through as a single comma-joined -tags
// flag — used only to select the e2e_scaledown build (see
// BuildAgentBinaryWithTags); every other caller passes none, so the binary
// every other scenario installs is unaffected by this parameter existing.
func buildCommand(ctx context.Context, moduleDir, outPath string, tags ...string) *exec.Cmd {
	args := []string{"build", "-o", outPath}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, "./cmd/mws-iops-autoscaler")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	return cmd
}
