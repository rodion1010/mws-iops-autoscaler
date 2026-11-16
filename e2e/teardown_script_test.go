//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRegression_Teardown_ParallelDiskDeletesAggregateFailures proves what
// bash -n cannot: teardown.sh's four disk deletes actually run concurrently
// rather than sequentially, and one disk's failure doesn't block or skip
// the others while still flipping the script's aggregate exit status
// (e2e/scripts/teardown.sh). A stub mws placed first on PATH stands in for
// the real CLI — infrastructure-free, like every other TestRegression_ test.
func TestRegression_Teardown_ParallelDiskDeletesAggregateFailures(t *testing.T) {
	dir := t.TempDir()
	const perCallDelay = time.Second
	writeStubMWS(t, dir, perCallDelay)

	cmd := exec.Command("bash", "teardown.sh")
	cmd.Dir = "scripts"
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"MWS_PROJECT=test-project",
		"E2E_RUN_ID=4242",
	)

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a non-zero exit status (the stub fails the noname disk delete), got success\noutput:\n%s", out)
	}

	// Sequential would take ~4*perCallDelay (4s); parallel takes
	// ~1*perCallDelay plus process overhead. 2.5x gives ample margin
	// without the test being sensitive to CI scheduling jitter.
	if elapsed > perCallDelay*5/2 {
		t.Fatalf("teardown.sh took %s for four disk deletes with a %s stub delay each — looks sequential, not parallel\noutput:\n%s", elapsed, perCallDelay, out)
	}

	for _, disk := range []string{"e2e-full-4242-boot", "e2e-full-4242-data", "e2e-full-4242-churn", "e2e-full-4242-noname"} {
		marker := filepath.Join(dir, "ran-"+disk)
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("disk %s was never attempted (marker missing): %v\noutput:\n%s", disk, statErr, out)
		}
	}
}

// writeStubMWS writes a fake mws CLI to dir that succeeds the VM delete
// immediately, sleeps delay then touches a "ran-<disk>" marker for every
// disk delete, and fails only the noname disk — proof that one failure
// doesn't short-circuit the others.
func writeStubMWS(t *testing.T, dir string, delay time.Duration) {
	t.Helper()
	script := fmt.Sprintf(`#!/usr/bin/env bash
if [ "$2" = "virtual-machine" ] && [ "$3" = "delete" ]; then
  exit 0
fi
if [ "$2" = "virtual-machine" ] && [ "$3" = "get" ]; then
  echo "ERROR: api error. Code: 404. Status: NOT_FOUND" >&2
  exit 1
fi
if [ "$2" = "disk" ] && [ "$3" = "delete" ]; then
  ref="$4"
  name="${ref##*/}"
  sleep %d
  touch "%s/ran-$name"
  case "$name" in
    *-noname) exit 1 ;;
    *) exit 0 ;;
  esac
fi
echo "stub mws: unexpected args: $*" >&2
exit 2
`, int(delay.Seconds()), dir)
	path := filepath.Join(dir, "mws")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub mws: %v", err)
	}
}

// writeRecordingMWS writes a fake mws CLI that records every delete and answers NOT_FOUND for the
// names listed in missing. A VM get reports the VM present for the first $STUB_VM_ALIVE_GETS calls
// (default 0) and NOT_FOUND after; STUB_VM_DELETE_FAILS makes the VM delete itself fail.
func writeRecordingMWS(t *testing.T, dir string, missing ...string) {
	t.Helper()
	script := fmt.Sprintf(`#!/usr/bin/env bash
ref="$4"; name="${ref##*/}"
if [ "$3" = get ]; then
  n=$(cat "%[1]s/gets" 2>/dev/null || echo 0); echo $((n + 1)) >"%[1]s/gets"
  [ "$n" -lt "${STUB_VM_ALIVE_GETS:-0}" ] && exit 0
  echo "ERROR: api error. Code: 404. Status: NOT_FOUND" >&2; exit 1
fi
echo "$2 $name" >>"%[1]s/calls"
if [ "$2" = virtual-machine ] && [ -n "${STUB_VM_DELETE_FAILS:-}" ]; then
  echo "ERROR: unexpected response for deletion" >&2; exit 1
fi
for m in %[2]s; do
  [ "$name" = "$m" ] && { echo "ERROR: api error. Code: 404. Status: NOT_FOUND" >&2; exit 1; }
done
exit 0
`, dir, strings.Join(missing, " "))
	if err := os.WriteFile(filepath.Join(dir, "mws"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub mws: %v", err)
	}
}

func runTeardown(t *testing.T, dir string, env ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("bash", "teardown.sh")
	cmd.Dir = "scripts"
	cmd.Env = append(append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "MWS_PROJECT=test-project"), env...)
	return cmd.CombinedOutput()
}

func TestRegression_Teardown_SmokeTierDeletesItsVMAndDisks(t *testing.T) {
	dir := t.TempDir()
	writeRecordingMWS(t, dir)
	out, err := runTeardown(t, dir, "E2E_TIER=smoke", "E2E_RUN_ID=4242", "E2E_RUN_ATTEMPT=2", "E2E_SCENARIO_ID=7")
	if err != nil {
		t.Fatalf("teardown.sh: %v\n%s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
	calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(calls) != 3 || calls[0] != "virtual-machine e2e-smoke-4242-2-s7" {
		t.Fatalf("calls = %q, want the VM first, then its boot and indep disks", calls)
	}
	for _, want := range []string{"disk e2e-smoke-4242-2-s7-boot", "disk e2e-smoke-4242-2-s7-indep"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("calls = %q, missing %q", calls, want)
		}
	}
}

// A provision that failed partway leaves some names never created; tearing it down must succeed.
func TestRegression_Teardown_AlreadyDeletedResourcesCountAsDeleted(t *testing.T) {
	dir := t.TempDir()
	writeRecordingMWS(t, dir, "e2e-smoke-4242-1-s0", "e2e-smoke-4242-1-s0-indep")
	out, err := runTeardown(t, dir, "E2E_TIER=smoke", "E2E_RUN_ID=4242", "E2E_RUN_ATTEMPT=1", "E2E_SCENARIO_ID=0")
	if err != nil {
		t.Fatalf("teardown.sh failed on NOT_FOUND resources: %v\n%s", err, out)
	}
}

func TestRegression_Teardown_RefusesMalformedIdentity(t *testing.T) {
	dir := t.TempDir()
	writeRecordingMWS(t, dir)
	if out, err := runTeardown(t, dir, "E2E_TIER=smoke", "E2E_RUN_ID=4242", "E2E_RUN_ATTEMPT=x", "E2E_SCENARIO_ID=0"); err == nil {
		t.Fatalf("teardown.sh accepted a non-numeric attempt\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls")); err == nil {
		t.Error("teardown.sh issued deletes for a malformed identity")
	}
}

// The VM delete call can fail while the platform keeps deleting the VM; its disks must wait for it.
func TestRegression_Teardown_WaitsForTheVMBeforeDeletingDisks(t *testing.T) {
	dir := t.TempDir()
	writeRecordingMWS(t, dir)
	out, err := runTeardown(t, dir, "E2E_TIER=smoke", "E2E_RUN_ID=4242", "E2E_RUN_ATTEMPT=1", "E2E_SCENARIO_ID=0",
		"STUB_VM_DELETE_FAILS=1", "STUB_VM_ALIVE_GETS=2", "TEARDOWN_POLL_SECONDS=0")
	if err != nil {
		t.Fatalf("teardown.sh failed although the VM disappeared: %v\n%s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(raw), "disk e2e-smoke-4242-1-s0-boot") || !strings.Contains(string(raw), "disk e2e-smoke-4242-1-s0-indep") {
		t.Fatalf("calls = %q, want both disks deleted once the VM was gone", raw)
	}
	if gets, _ := os.ReadFile(filepath.Join(dir, "gets")); strings.TrimSpace(string(gets)) != "3" {
		t.Errorf("VM gets = %q, want 3 (two present, then gone)", gets)
	}
}

func TestRegression_Teardown_VMThatNeverDisappearsLeavesItsDisks(t *testing.T) {
	dir := t.TempDir()
	writeRecordingMWS(t, dir)
	out, err := runTeardown(t, dir, "E2E_TIER=smoke", "E2E_RUN_ID=4242", "E2E_RUN_ATTEMPT=1", "E2E_SCENARIO_ID=0",
		"STUB_VM_ALIVE_GETS=100", "TEARDOWN_POLL_SECONDS=0", "TEARDOWN_POLL_ATTEMPTS=3")
	if err == nil {
		t.Fatalf("teardown.sh succeeded while the VM still existed\n%s", out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(raw), "disk ") {
		t.Errorf("calls = %q, want no disk deletes while the VM exists", raw)
	}
}
