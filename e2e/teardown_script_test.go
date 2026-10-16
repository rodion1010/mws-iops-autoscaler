//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		"E2E_RUN_ID=test-run",
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

	for _, disk := range []string{"e2e-full-test-run-boot", "e2e-full-test-run-data", "e2e-full-test-run-churn", "e2e-full-test-run-noname"} {
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
