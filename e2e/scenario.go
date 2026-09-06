//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
)

// remoteWorkDir is scratch space owned exclusively by the E2E harness.
const remoteWorkDir = "/tmp/mws-iops-e2e"

const recoveryTimeout = 2 * time.Minute

func recoveryContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), recoveryTimeout)
}

// e2eScaleUpWindow drives both agent configuration and result validation.
const e2eScaleUpWindow = 30 * time.Second

// agentConfigYAML uses shorter windows and thresholds calibrated for the
// harness's single-process fio load; production defaults remain unchanged.
func agentConfigYAML(t Target) string {
	return fmt.Sprintf(`apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local

policy:
  limits: {min_iops: 1000, max_iops: 10000}
  advanced:
    enabled: true
    evaluation: {scale_up_window: %s, scale_down_window: 60s}
    saturation: {utilization_threshold: 0.90, queue_depth_threshold: 0.5}
    cooldown: {scale_up: 10s, scale_down: 20s}

disks:
  mode: selected
  selected:
    - device_name: %s

runtime:
  metrics: {source: guest, collection_interval: 5s}
  controller: {reconcile_interval: 10s, discovery_interval: 5m}
`, e2eScaleUpWindow, t.DeviceName)
}

// Bracketed patterns prevent pkill from matching its own command line.
func killAgentCmd(signal string) string {
	sig := ""
	if signal != "" {
		sig = "-" + signal + " "
	}
	return "pkill " + sig + "-f '" + remoteWorkDir + "/[m]ws-iops-autoscaler' || true"
}

func killLoadCmd() string {
	// DriveLoad creates a root-owned fio process.
	return "sudo pkill -f '[f]io.*e2e-loadtest' || true"
}

// runner runs a command on the target guest OS and returns its combined
// output. Target satisfies it via its own Run; regression tests script it.
type runner interface {
	Run(ctx context.Context, remoteCmd string) (string, error)
}

// Markers keep the result parseable when SSH adds warnings to combined output.
const (
	agentPresentMarker    = "mws-e2e-agent-present"
	agentAbsentMarker     = "mws-e2e-agent-absent"
	agentPgrepErrorPrefix = "mws-e2e-agent-pgrep-error-"
)

// agentAbsentCmd distinguishes present, absent, and pgrep failure explicitly.
func agentAbsentCmd() string {
	return "pgrep -f '" + remoteWorkDir + "/[m]ws-iops-autoscaler' >/dev/null; rc=$?; " +
		"case $rc in " +
		"0) echo " + agentPresentMarker + ";; " +
		"1) echo " + agentAbsentMarker + ";; " +
		"*) echo " + agentPgrepErrorPrefix + "$rc; exit $rc;; " +
		"esac"
}

// parseAgentPresence reads the final marker and fails closed on other output.
func parseAgentPresence(out string) (absent bool, err error) {
	var last string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			last = s
		}
	}
	switch {
	case last == "":
		return false, fmt.Errorf("no output")
	case last == agentPresentMarker:
		return false, nil
	case last == agentAbsentMarker:
		return true, nil
	case strings.HasPrefix(last, agentPgrepErrorPrefix):
		return false, fmt.Errorf("remote pgrep failed: %s", last)
	default:
		return false, fmt.Errorf("unrecognized output %q", out)
	}
}

// agentStopTiming is injectable so regression tests can avoid real delays.
type agentStopTiming struct {
	termAttempts int
	killAttempts int
	interval     time.Duration
}

// Escalation is safe because state is crash-safe and the E2E directory is disposable.
var defaultAgentStopTiming = agentStopTiming{termAttempts: 2, killAttempts: 2, interval: 2 * time.Second}

// stopStaleAgent verifies termination and escalates from SIGTERM to SIGKILL.
func stopStaleAgent(ctx context.Context, r runner, timing agentStopTiming) error {
	rounds := []struct {
		signal   string
		attempts int
	}{
		{"TERM", timing.termAttempts},
		{"KILL", timing.killAttempts},
	}
	for _, round := range rounds {
		for attempt := 1; attempt <= round.attempts; attempt++ {
			if _, err := r.Run(ctx, killAgentCmd(round.signal)); err != nil {
				return fmt.Errorf("e2e: stop stale agent: %w", err)
			}
			out, err := r.Run(ctx, agentAbsentCmd())
			if err != nil {
				return fmt.Errorf("e2e: stop stale agent: verify absence: %w", err)
			}
			absent, err := parseAgentPresence(out)
			if err != nil {
				return fmt.Errorf("e2e: stop stale agent: verify absence: %w", err)
			}
			if absent {
				return nil
			}
			if attempt < round.attempts {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(timing.interval):
				}
			}
		}
	}
	return fmt.Errorf("e2e: stop stale agent: e2e agent process still present after %d SIGTERM and %d SIGKILL kill-and-verify rounds",
		timing.termAttempts, timing.killAttempts)
}

// Preflight recovers the shared target from an interrupted previous run.
func Preflight(ctx context.Context, client compute.Client, t Target) error {
	if err := stopStaleAgent(ctx, t, defaultAgentStopTiming); err != nil {
		return fmt.Errorf("e2e: preflight: %w", err)
	}
	if _, err := t.Run(ctx, killLoadCmd()); err != nil {
		return fmt.Errorf("e2e: preflight: stop stale fio: %w", err)
	}
	if _, err := t.Run(ctx, "rm -rf "+remoteWorkDir+" && mkdir -p "+remoteWorkDir); err != nil {
		return fmt.Errorf("e2e: preflight: reset work dir: %w", err)
	}
	if err := AssertDiskClass(ctx, client, t); err != nil {
		return fmt.Errorf("e2e: preflight: %w", err)
	}
	if err := ResetDiskToBaseline(ctx, client, t); err != nil {
		return fmt.Errorf("e2e: preflight: %w", err)
	}
	return nil
}

// Cleanup attempts every recovery step and joins their errors.
func Cleanup(ctx context.Context, client compute.Client, t Target) error {
	var errs []error
	if err := stopStaleAgent(ctx, t, defaultAgentStopTiming); err != nil {
		errs = append(errs, fmt.Errorf("stop stale agent: %w", err))
	}
	if _, err := t.Run(ctx, killLoadCmd()); err != nil {
		errs = append(errs, fmt.Errorf("stop stale fio: %w", err))
	}
	if err := ResetDiskToBaseline(ctx, client, t); err != nil {
		errs = append(errs, fmt.Errorf("reset disk to baseline: %w", err))
	}
	return errors.Join(errs...)
}

// InstallAgent copies localBinaryPath and a generated config to t's guest OS.
func InstallAgent(ctx context.Context, t Target, localBinaryPath string) error {
	if err := t.Copy(ctx, localBinaryPath, remoteWorkDir+"/mws-iops-autoscaler"); err != nil {
		return fmt.Errorf("e2e: install agent binary: %w", err)
	}
	if _, err := t.Run(ctx, "chmod +x "+remoteWorkDir+"/mws-iops-autoscaler"); err != nil {
		return fmt.Errorf("e2e: install agent binary: %w", err)
	}

	tmp, err := os.CreateTemp("", "e2e-agent-config-*.yaml")
	if err != nil {
		return fmt.Errorf("e2e: install agent config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(agentConfigYAML(t)); err != nil {
		tmp.Close()
		return fmt.Errorf("e2e: install agent config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("e2e: install agent config: %w", err)
	}
	if err := t.Copy(ctx, tmp.Name(), remoteWorkDir+"/config.yaml"); err != nil {
		return fmt.Errorf("e2e: install agent config: %w", err)
	}
	return nil
}

// StartAgent forwards SDK scope explicitly and confines mutable state to the
// disposable remote work directory.
func StartAgent(ctx context.Context, t Target, project string) error {
	// env works when RunBackground places the command directly after nohup.
	envAssign := "MWS_PROJECT=" + project
	if zone := Zone(); zone != "" {
		envAssign += " MWS_ZONE=" + zone
	}
	cmd := fmt.Sprintf(
		"env %s %s/mws-iops-autoscaler -config %s/config.yaml -state-dir %s/state -journal %s/journal.jsonl -listen :9435",
		envAssign, remoteWorkDir, remoteWorkDir, remoteWorkDir, remoteWorkDir,
	)
	return t.RunBackground(ctx, cmd)
}

// StopAgent requests a graceful shutdown.
func StopAgent(ctx context.Context, t Target) error {
	_, err := t.Run(ctx, killAgentCmd("TERM")+"; sleep 2")
	return err
}

// DriveLoad tags its root-owned fio process for scoped cleanup.
func DriveLoad(ctx context.Context, t Target) error {
	guestPath := (compute.StorageDisk{DeviceName: t.DeviceName}).GuestPath()
	cmd := fmt.Sprintf(
		"sudo fio --name=e2e-loadtest --filename=%s --rw=randread --bs=4k --iodepth=64 --direct=1 --time_based=1 --runtime=600",
		guestPath,
	)
	return t.RunBackground(ctx, cmd)
}

// WaitForScaleUp requires an observed baseline and rejects changes that occur
// before the agent's evaluation window could have produced them.
func WaitForScaleUp(ctx context.Context, client compute.Client, t Target, timeout time.Duration, agentStarted time.Time) (int64, error) {
	return waitForScaleUp(ctx, client, t, timeout, e2eScaleUpWindow, 5*time.Second, agentStarted)
}

// waitForScaleUp exposes timing for deterministic regression tests.
func waitForScaleUp(ctx context.Context, client compute.Client, t Target, timeout, minScaleUp, pollInterval time.Duration, agentStarted time.Time) (int64, error) {
	deadline := time.Now().Add(timeout)
	observedBaseline := false
	for {
		current, err := CurrentIOPS(ctx, client, t)
		if err != nil {
			return 0, fmt.Errorf("e2e: wait for scale-up: %w", err)
		}
		if !observedBaseline {
			if current != t.BaselineIOPS {
				return 0, fmt.Errorf("e2e: wait for scale-up: precondition: first observation is %d, want baseline %d — the disk is already off baseline (leftover elevation from a prior run or a stale read); refusing to credit it as scale-up",
					current, t.BaselineIOPS)
			}
			observedBaseline = true
		} else if current > t.BaselineIOPS {
			if elapsed := time.Since(agentStarted); elapsed < minScaleUp {
				return 0, fmt.Errorf("e2e: wait for scale-up: observed %d above baseline %d after %s — before the %s scale-up window could elapse since the agent started, so this cannot be this scenario's own scale-up (leftover elevation or a racing writer); failing closed",
					current, t.BaselineIOPS, elapsed, minScaleUp)
			}
			return current, nil
		}
		if time.Now().After(deadline) {
			return current, fmt.Errorf("e2e: wait for scale-up: still at %d after %s (baseline %d)", current, timeout, t.BaselineIOPS)
		}
		select {
		case <-ctx.Done():
			return current, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// FetchJournal returns diagnostic output; it does not determine success.
func FetchJournal(ctx context.Context, t Target) string {
	out, err := t.Run(ctx, "cat "+remoteWorkDir+"/journal.jsonl 2>/dev/null || true")
	if err != nil {
		return fmt.Sprintf("(failed to fetch journal: %v)", err)
	}
	return out
}

// BuildAgentBinary cross-compiles the agent for the target's guest OS
// (linux/amd64 — every MWS VM shape this project targets) into a temp file
// and returns its path. The caller is responsible for removing it.
func BuildAgentBinary(ctx context.Context, moduleDir string) (string, error) {
	out := filepath.Join(os.TempDir(), "mws-iops-autoscaler-e2e")
	cmd := buildCommand(ctx, moduleDir, out)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("e2e: build agent binary: %w: %s", err, output)
	}
	return out, nil
}
