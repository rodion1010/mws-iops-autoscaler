//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// e2eScaleDownWindow mirrors e2eScaleUpWindow for the down direction. It
// must stay >= e2eScaleUpWindow (internal/config/validate.go's
// scale_down_window < scale_up_window rule) — 60s against 30s leaves that
// margin without materially slowing the scale-down scenarios down.
const e2eScaleDownWindow = 60 * time.Second

// defaultDiscoveryInterval is agentConfigYAML's discovery_interval, unchanged
// since before DisksSpec existed. Every existing scenario relies on it.
const defaultDiscoveryInterval = 5 * time.Minute

// discoveryScenarioInterval is for scenarios that must observe rediscovery
// within a test's lifetime (e.g. an attach picked up without a restart).
// main.go derives reconcile()'s fail-closed staleness guard, maxDiscoveryAge,
// as 3x discovery_interval, so this cannot become defaultDiscoveryInterval:
// doing so would shrink every existing scenario's margin from 15 minutes
// (never observed to fire) to 90 seconds, risking a spurious failure in the
// multi-minute P1-P5 load scenarios if any single discovery cycle ever
// stalled that long. 30s keeps that margin at 90s — ample slack for a single
// compute-API call — while bounding how long a discovery scenario waits.
const discoveryScenarioInterval = 30 * time.Second

// DisksSpec configures the generated agent config's disks: block, mirroring
// internal/config.DisksSpec's shape without importing it — e2e authors the
// on-disk YAML contract the same way any other config consumer would.
type DisksSpec struct {
	Mode     string
	Selected []string
	Exclude  []string
}

// SelectedDisksSpec is agentConfigYAML's original, still-default shape: every
// name in mode: selected.
func SelectedDisksSpec(deviceNames ...string) DisksSpec {
	return DisksSpec{Mode: "selected", Selected: deviceNames}
}

// AutoDiscoveryDisksSpec is mode: auto-discovery, optionally excluding names.
func AutoDiscoveryDisksSpec(exclude ...string) DisksSpec {
	return DisksSpec{Mode: "auto-discovery", Exclude: exclude}
}

func disksBlockYAML(spec DisksSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "disks:\n  mode: %s\n", spec.Mode)
	switch spec.Mode {
	case "selected":
		b.WriteString("  selected:\n")
		for _, name := range spec.Selected {
			fmt.Fprintf(&b, "    - device_name: %s\n", name)
		}
	case "auto-discovery":
		if len(spec.Exclude) > 0 {
			b.WriteString("  exclude:\n")
			for _, name := range spec.Exclude {
				fmt.Fprintf(&b, "    - %s\n", name)
			}
		}
	}
	return b.String()
}

// agentConfigYAML is InstallAgent's default rendering: mode: selected for
// exactly t.DeviceName at the production discovery_interval default.
func agentConfigYAML(t Target) string {
	return agentConfigYAMLWithDisks(SelectedDisksSpec(t.DeviceName), defaultDiscoveryInterval)
}

// agentConfigYAMLWithDisks renders the harness's config with shorter windows
// and thresholds calibrated for its single-process fio load (production
// defaults remain unchanged), an explicit disks: selection, and an explicit
// discovery_interval — see discoveryScenarioInterval's comment for why a
// caller must not casually pass a value faster than defaultDiscoveryInterval.
func agentConfigYAMLWithDisks(disks DisksSpec, discoveryInterval time.Duration) string {
	return fmt.Sprintf(`apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local

policy:
  limits: {min_iops: 1000, max_iops: 10000}
  advanced:
    enabled: true
    evaluation: {scale_up_window: %s, scale_down_window: %s}
    saturation: {utilization_threshold: 0.90, queue_depth_threshold: 0.5}
    cooldown: {scale_up: 10s, scale_down: 20s}

%s
runtime:
  metrics: {source: guest, collection_interval: 5s}
  controller: {reconcile_interval: 10s, discovery_interval: %s}
`, e2eScaleUpWindow, e2eScaleDownWindow, disksBlockYAML(disks), discoveryInterval)
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

// InstallAgent copies localBinaryPath and a generated config to t's guest OS,
// selecting exactly t.DeviceName at the production discovery_interval
// default — the shape every existing scenario relies on, unchanged by
// InstallAgentWithDisks's addition.
func InstallAgent(ctx context.Context, t Target, localBinaryPath string) error {
	return InstallAgentWithDisks(ctx, t, localBinaryPath, SelectedDisksSpec(t.DeviceName), defaultDiscoveryInterval)
}

// InstallAgentWithDisks is InstallAgent for a scenario that needs an
// explicit disks: selection and discovery_interval — the discovery/scope
// scenarios, which must observe rediscovery within the test's lifetime.
func InstallAgentWithDisks(ctx context.Context, t Target, localBinaryPath string, disks DisksSpec, discoveryInterval time.Duration) error {
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
	if _, err := tmp.WriteString(agentConfigYAMLWithDisks(disks, discoveryInterval)); err != nil {
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

// agentLogPath is a stable path (unlike RunBackground's auto-derived one) so
// a scenario that expects the agent to keep running can still read back why
// it didn't (FetchAgentLog) — e.g. a cold-start-fatal discovery exits the
// whole process almost immediately (docs/design.md §4.1).
const agentLogPath = remoteWorkDir + "/agent.log"

// StartAgent confines mutable state to the disposable remote work
// directory and appends US-013's -dry-run flag when dryRun is set, so the
// process discovers, samples and decides but applies no change.
//
// Neither MWS_PROJECT nor MWS_ZONE is forwarded: local mode resolves its
// own project and zone from the VM's instance metadata, and it no longer
// reads either variable at all, so forwarding one would only pass an
// environment override the agent doesn't act on.
func StartAgent(ctx context.Context, t Target, dryRun bool) error {
	cmd := fmt.Sprintf(
		"%s/mws-iops-autoscaler -config %s/config.yaml -state-dir %s/state -journal %s/journal.jsonl -listen :9435",
		remoteWorkDir, remoteWorkDir, remoteWorkDir, remoteWorkDir,
	)
	if dryRun {
		cmd += " -dry-run"
	}
	return t.RunBackgroundToLog(ctx, cmd, agentLogPath)
}

// FetchAgentLog returns the agent's captured stdout/stderr — diagnostic
// only, the same convention as FetchJournal.
func FetchAgentLog(ctx context.Context, t Target) string {
	out, err := t.Run(ctx, "cat "+agentLogPath+" 2>/dev/null || true")
	if err != nil {
		return fmt.Sprintf("(failed to fetch agent log: %v)", err)
	}
	return out
}

// StopAgent requests a graceful shutdown.
func StopAgent(ctx context.Context, t Target) error {
	_, err := t.Run(ctx, killAgentCmd("TERM")+"; sleep 2")
	return err
}

// loadLogPath is a stable path (mirroring agentLogPath) so a scenario whose
// expected saturation/demand never shows up can tell whether fio actually
// ran for its full --runtime=600 or died immediately (e.g. a bad guest path)
// — FetchLoadLog reads it back. All three Drive*Load functions share it:
// only one load generator runs at a time per scenario.
const loadLogPath = remoteWorkDir + "/load.log"

// FetchLoadLog returns the fio job's captured stdout/stderr — diagnostic
// only, the same convention as FetchAgentLog.
func FetchLoadLog(ctx context.Context, t Target) string {
	out, err := t.Run(ctx, "cat "+loadLogPath+" 2>/dev/null || true")
	if err != nil {
		return fmt.Sprintf("(failed to fetch load log: %v)", err)
	}
	return out
}

// DriveLoad tags its root-owned fio process for scoped cleanup.
func DriveLoad(ctx context.Context, t Target) error {
	guestPath := (compute.StorageDisk{DeviceName: t.DeviceName}).GuestPath()
	cmd := fmt.Sprintf(
		"sudo fio --name=e2e-loadtest --filename=%s --rw=randread --bs=4k --ioengine=libaio --iodepth=64 --direct=1 --time_based=1 --runtime=600",
		guestPath,
	)
	return t.RunBackgroundToLog(ctx, cmd, loadLogPath)
}

// DriveRateLimitedLoad caps ops well under the saturation gate (this
// harness's config uses a 0.90 utilization threshold against a 1000-IOPS
// baseline, i.e. 900 ops) so a scenario can drive real, sustained I/O
// without ever crossing into saturation. Shares DriveLoad's job name and
// guest path convention so killLoadCmd and DriveLoad's own cleanup cover it
// identically.
func DriveRateLimitedLoad(ctx context.Context, t Target, rateIOPS int) error {
	guestPath := (compute.StorageDisk{DeviceName: t.DeviceName}).GuestPath()
	cmd := fmt.Sprintf(
		"sudo fio --name=e2e-loadtest --filename=%s --rw=randread --bs=4k --ioengine=libaio --iodepth=1 --direct=1 --time_based=1 --runtime=600 --rate_iops=%d",
		guestPath, rateIOPS,
	)
	return t.RunBackgroundToLog(ctx, cmd, loadLogPath)
}

// DriveBandwidthLoad uses a large block size at depth to saturate bandwidth
// while keeping ops well below what an ops-denominated demand estimate
// would read as saturated — the fingerprint sustainedResourceGate's
// bandwidth branch (internal/analyzer) exists to catch. It is DriveLoad's
// only difference: same job name, same guest path, a larger block size.
func DriveBandwidthLoad(ctx context.Context, t Target) error {
	guestPath := (compute.StorageDisk{DeviceName: t.DeviceName}).GuestPath()
	cmd := fmt.Sprintf(
		"sudo fio --name=e2e-loadtest --filename=%s --rw=randread --bs=64k --ioengine=libaio --iodepth=64 --direct=1 --time_based=1 --runtime=600",
		guestPath,
	)
	return t.RunBackgroundToLog(ctx, cmd, loadLogPath)
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

// WaitForScaleDown is WaitForScaleUp's mirror: it requires an observed
// baseline and rejects a drop that occurs before the agent's scale-down
// window could have produced it.
func WaitForScaleDown(ctx context.Context, client compute.Client, t Target, timeout time.Duration, agentStarted time.Time) (int64, error) {
	return waitForScaleDown(ctx, client, t, timeout, e2eScaleDownWindow, 5*time.Second, agentStarted)
}

// waitForScaleDown exposes timing for deterministic regression tests, mirroring waitForScaleUp.
func waitForScaleDown(ctx context.Context, client compute.Client, t Target, timeout, minScaleDown, pollInterval time.Duration, agentStarted time.Time) (int64, error) {
	deadline := time.Now().Add(timeout)
	observedBaseline := false
	for {
		current, err := CurrentIOPS(ctx, client, t)
		if err != nil {
			return 0, fmt.Errorf("e2e: wait for scale-down: %w", err)
		}
		if !observedBaseline {
			if current != t.BaselineIOPS {
				return 0, fmt.Errorf("e2e: wait for scale-down: precondition: first observation is %d, want baseline %d — the disk is already off baseline (leftover elevation from a prior run or a stale read); refusing to credit it as scale-down",
					current, t.BaselineIOPS)
			}
			observedBaseline = true
		} else if current < t.BaselineIOPS {
			if elapsed := time.Since(agentStarted); elapsed < minScaleDown {
				return 0, fmt.Errorf("e2e: wait for scale-down: observed %d below baseline %d after %s — before the %s scale-down window could elapse since the agent started, so this cannot be this scenario's own scale-down (leftover reduction or a racing writer); failing closed",
					current, t.BaselineIOPS, elapsed, minScaleDown)
			}
			return current, nil
		}
		if time.Now().After(deadline) {
			return current, fmt.Errorf("e2e: wait for scale-down: still at %d after %s (baseline %d)", current, timeout, t.BaselineIOPS)
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

// journalRecord decodes only the journal fields e2e scenarios need to
// assert on. It mirrors internal/journal.Record's field names rather than
// importing that type — the journal is an external contract (design.md
// §7.2, internal/journal.go's own doc comment), so an e2e scenario reads it
// the same way any other external consumer would.
type journalRecord struct {
	Action         string  `json:"action"`
	Reason         string  `json:"reason"`
	CurrentIOPS    int64   `json:"current_iops"`
	TargetIOPS     int64   `json:"target_iops"`
	Utilization    float64 `json:"utilization"`
	VMHeadroomIOPS int64   `json:"vm_headroom_iops"`
}

// journalRecords fetches and decodes every well-formed line the guest's
// journal currently holds. A line that isn't a JSON record (an ssh
// host-key warning, a partial write) is skipped rather than failing the
// whole read on it.
func journalRecords(ctx context.Context, t Target) ([]journalRecord, error) {
	out, err := t.Run(ctx, "cat "+remoteWorkDir+"/journal.jsonl 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("fetch journal: %w", err)
	}
	var recs []journalRecord
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec journalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// WaitForJournalRecord polls the remote decision journal until some record
// satisfies match, or fails at timeout. Preflight resets the guest's
// journal file for every scenario, so any record found here was produced
// by this run — no need to additionally filter by timestamp.
func WaitForJournalRecord(ctx context.Context, t Target, timeout, pollInterval time.Duration, match func(journalRecord) bool) (journalRecord, error) {
	deadline := time.Now().Add(timeout)
	for {
		recs, err := journalRecords(ctx, t)
		if err != nil {
			return journalRecord{}, fmt.Errorf("e2e: wait for journal record: %w", err)
		}
		for _, rec := range recs {
			if match(rec) {
				return rec, nil
			}
		}
		if time.Now().After(deadline) {
			return journalRecord{}, fmt.Errorf("e2e: wait for journal record: no matching record in the journal after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return journalRecord{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// WaitForDecision polls the remote decision journal until a record shows
// wantAction, or fails at timeout. This is how a dry run's decision is
// observed: US-013 applies no change, so CurrentIOPS/WaitForScaleUp have
// nothing to detect, but the journal is written unconditionally before
// actuation (design.md §7.2) whether or not the write itself is real.
func WaitForDecision(ctx context.Context, t Target, timeout, pollInterval time.Duration, wantAction string) error {
	_, err := WaitForJournalRecord(ctx, t, timeout, pollInterval, func(rec journalRecord) bool {
		return rec.Action == wantAction
	})
	return err
}

// AssertNoIOPSChange polls until duration elapses and fails if the disk is
// ever observed off baseline — the dry-run counterpart to WaitForScaleUp,
// which expects a rise. Zero mutation is confirmed positively, through
// repeated fresh reads, rather than assumed from a single reading.
func AssertNoIOPSChange(ctx context.Context, client compute.Client, t Target, duration, pollInterval time.Duration) error {
	deadline := time.Now().Add(duration)
	for {
		current, err := CurrentIOPS(ctx, client, t)
		if err != nil {
			return fmt.Errorf("e2e: assert no IOPS change: %w", err)
		}
		if current != t.BaselineIOPS {
			return fmt.Errorf("e2e: assert no IOPS change: observed %d, want baseline %d — a dry run must apply no change", current, t.BaselineIOPS)
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// countActuatingTransitions counts, in journal order, how many times an
// actuating record's Action differs from the previous one — skipping
// no_action/vm_cap_reached records, which never actuate.
func countActuatingTransitions(recs []journalRecord) int {
	transitions := 0
	last := ""
	for _, rec := range recs {
		if rec.Action != "scale_up" && rec.Action != "scale_down" {
			continue
		}
		if last != "" && rec.Action != last {
			transitions++
		}
		last = rec.Action
	}
	return transitions
}

// AssertBoundedFlapping fetches the current journal and fails if the number
// of alternating scale_up/scale_down actuations exceeds maxTransitions
// (design.md §8.3's "no flapping" scenario). A point-in-time check, called
// after a scenario's own load pattern has already run to completion.
func AssertBoundedFlapping(ctx context.Context, t Target, maxTransitions int) error {
	recs, err := journalRecords(ctx, t)
	if err != nil {
		return fmt.Errorf("e2e: assert bounded flapping: %w", err)
	}
	if got := countActuatingTransitions(recs); got > maxTransitions {
		return fmt.Errorf("e2e: assert bounded flapping: %d scale_up/scale_down alternations, want <= %d — hysteresis/cooldown should have suppressed this", got, maxTransitions)
	}
	return nil
}

// runAgentScenario builds, installs and starts the agent against target,
// calls run once it is up, and stops it afterward — the sequence every
// scenario test in this package repeats identically before diverging into
// its own load and assertions. Preflight and Cleanup deliberately stay at
// each call site rather than moving in here: the full tier skips Cleanup
// (its VM is torn down instead) while the smoke tier does not, so neither
// belongs in a helper both tiers share.
//
// run receives the moment the agent started — the same origin
// WaitForScaleUp's window-timing guard needs. Any load a scenario drives
// before this point (a cold-start scenario driving load before the agent
// even starts) must be issued by the caller before calling
// runAgentScenario, never inside run: moving it into run would silently
// change what's being tested.
//
// On a run error, the remote journal is fetched and folded into the
// failure for diagnostics, regardless of which step inside run failed.
func runAgentScenario(t *testing.T, ctx context.Context, target Target, project string, dryRun bool, run func(agentStarted time.Time) error) {
	t.Helper()
	runAgentScenarioWithDisks(t, ctx, target, project, dryRun, SelectedDisksSpec(target.DeviceName), defaultDiscoveryInterval, run)
}

// runAgentScenarioWithDisks is runAgentScenario for a scenario that needs an
// explicit disks: selection and discovery_interval (see
// InstallAgentWithDisks and discoveryScenarioInterval).
func runAgentScenarioWithDisks(t *testing.T, ctx context.Context, target Target, project string, dryRun bool, disks DisksSpec, discoveryInterval time.Duration, run func(agentStarted time.Time) error) {
	t.Helper()
	if err := runAgentScenarioWithDisksErr(ctx, target, project, dryRun, disks, discoveryInterval, run); err != nil {
		t.Fatalf("%v", err)
	}
}

// runAgentScenarioWithDisksErr is runAgentScenarioWithDisks with every step
// returning an error instead of calling t.Fatalf — the primitive an xfail
// scenario needs (see xfail's doc comment), since Go gives no way to catch a
// *testing.T failure raised from inside a test. runAgentScenarioWithDisks is
// the *testing.T-facing wrapper every other scenario keeps using unchanged.
func runAgentScenarioWithDisksErr(ctx context.Context, target Target, project string, dryRun bool, disks DisksSpec, discoveryInterval time.Duration, run func(agentStarted time.Time) error) error {
	binPath, err := BuildAgentBinary(ctx, "..")
	if err != nil {
		return fmt.Errorf("build agent binary: %w", err)
	}
	defer os.Remove(binPath)

	if err := InstallAgentWithDisks(ctx, target, binPath, disks, discoveryInterval); err != nil {
		return fmt.Errorf("install agent: %w", err)
	}
	if err := StartAgent(ctx, target, dryRun); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}
	agentStarted := time.Now()
	defer func() {
		stopCtx, stopCancel := recoveryContext()
		defer stopCancel()
		// Best-effort cleanup only — there is no *testing.T here to log a
		// failure to, and a failure to stop is never this function's own
		// error to report (the caller's run() error, if any, already is).
		_ = StopAgent(stopCtx, target)
		// killLoadCmd matches by fio job name, not by guest device, so this
		// also reaches a Drive*Load call a run() made against some other
		// Target on the same shared VM (e.g. a churn/sibling disk). Without
		// this, a scenario that fails or times out leaves its fio running
		// for the rest of its --runtime=600, contending for CPU/IO with
		// whatever full-tier test runs next.
		_, _ = target.Run(stopCtx, killLoadCmd())
	}()

	if err := run(agentStarted); err != nil {
		diagnosticCtx, diagnosticCancel := recoveryContext()
		journal := FetchJournal(diagnosticCtx, target)
		loadLog := FetchLoadLog(diagnosticCtx, target)
		diagnosticCancel()
		return fmt.Errorf("%w\njournal:\n%s\nload log:\n%s", err, journal, loadLog)
	}
	return nil
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
