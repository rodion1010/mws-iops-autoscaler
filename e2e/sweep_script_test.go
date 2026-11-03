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

// sweepNow is the clock the sweep tests run at; resources are aged relative to it.
var sweepNow = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

// sweepFixture is a stub mws CLI on PATH backed by a directory of list responses, so sweep.sh runs
// without a cloud project (e2e/scripts/sweep.sh). A delete drops the resource from later lists.
type sweepFixture struct {
	t     *testing.T
	state string
	bin   string
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	f := &sweepFixture{t: t, state: t.TempDir(), bin: t.TempDir()}
	stub := `#!/usr/bin/env bash
kind="$2"; verb="$3"
case "$verb" in
list)
  [ -f "$STUB_DIR/fail-list-$kind" ] && { echo "stub: list failed" >&2; exit 1; }
  deleted=$(cd "$STUB_DIR" && ls deleted-* 2>/dev/null | sed 's/^deleted-//')
  jq -c --arg deleted "$deleted" '.items |= map(select((.metadata.id | split("/") | last) as $n | ($deleted | split("\n") | index($n)) | not))' "$STUB_DIR/$kind.json" ;;
delete)
  name="${4##*/}"
  echo "$kind $name ${*:5}" >> "$STUB_DIR/calls"
  [ -f "$STUB_DIR/fail-delete-$name" ] && exit 1
  touch "$STUB_DIR/deleted-$name" ;;
*) echo "stub mws: unexpected args: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(f.bin, "mws"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub mws: %v", err)
	}
	return f
}

// resources writes the list response for a kind: name -> age before sweepNow, or a raw createTime
// when the value is a string.
func (f *sweepFixture) resources(kind, segment string, items map[string]any) {
	f.t.Helper()
	var parts []string
	for name, age := range items {
		created := ""
		switch v := age.(type) {
		case time.Duration:
			created = sweepNow.Add(-v).Format(time.RFC3339)
		case string:
			created = v
		}
		parts = append(parts, fmt.Sprintf(`{"metadata":{"id":"compute/projects/p/%s/%s","createTime":%q}}`, segment, name, created))
	}
	body := `{"items":[` + strings.Join(parts, ",") + `]}`
	if err := os.WriteFile(filepath.Join(f.state, kind+".json"), []byte(body), 0o644); err != nil {
		f.t.Fatalf("write %s list: %v", kind, err)
	}
}

func (f *sweepFixture) touch(name string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.state, name), nil, 0o644); err != nil {
		f.t.Fatalf("touch %s: %v", name, err)
	}
}

// run executes sweep.sh and returns its output, exit error and the delete calls recorded so far.
func (f *sweepFixture) run(apply bool) (out string, err error, calls []string) {
	f.t.Helper()
	cmd := exec.Command("bash", "sweep.sh")
	cmd.Dir = "scripts"
	cmd.Env = append(os.Environ(),
		"PATH="+f.bin+":"+os.Getenv("PATH"),
		"STUB_DIR="+f.state,
		"MWS_PROJECT=p",
		fmt.Sprintf("SWEEP_NOW=%d", sweepNow.Unix()),
		fmt.Sprintf("SWEEP_APPLY=%t", apply),
	)
	b, err := cmd.CombinedOutput()
	if raw, rerr := os.ReadFile(filepath.Join(f.state, "calls")); rerr == nil {
		calls = strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	return string(b), err, calls
}

func (f *sweepFixture) clearCalls() { _ = os.Remove(filepath.Join(f.state, "calls")) }

func TestRegression_Sweep_DeletesOnlyOwnedResourcesOlderThanTTL(t *testing.T) {
	f := newSweepFixture(t)
	f.resources("virtual-machine", "virtualMachines", map[string]any{
		"e2e-full-111": 5 * time.Hour,
		"e2e-full-222": 90 * time.Minute, // younger than the 3h TTL
		"ci-runner":    400 * time.Hour,  // not ours, however old
	})
	f.resources("disk", "disks", map[string]any{
		"e2e-full-111-boot": 5 * time.Hour,
		"e2e-full-111-data": 5 * time.Hour,
		"e2e-full-222-boot": 90 * time.Minute,
		"ci-runner-boot":    400 * time.Hour,
		"standalone-test":   400 * time.Hour,
	})

	out, err, calls := f.run(true)
	if err != nil {
		t.Fatalf("sweep.sh failed: %v\n%s", err, out)
	}

	want := map[string]bool{"virtual-machine e2e-full-111 --wait-timeout 5m": true,
		"disk e2e-full-111-boot --purge --wait-timeout 2m": true,
		"disk e2e-full-111-data --purge --wait-timeout 2m": true}
	if len(calls) != len(want) {
		t.Fatalf("delete calls = %q, want exactly %d (old owned resources only)\n%s", calls, len(want), out)
	}
	for _, c := range calls {
		if !want[c] {
			t.Errorf("unexpected delete call %q\n%s", c, out)
		}
	}
	// A disk cannot be deleted while its VM exists, so every VM delete comes first.
	if !strings.HasPrefix(calls[0], "virtual-machine ") {
		t.Errorf("first delete = %q, want the VM before any disk", calls[0])
	}
}

func TestRegression_Sweep_DryRunReportsCandidatesAndDeletesNothing(t *testing.T) {
	f := newSweepFixture(t)
	f.resources("virtual-machine", "virtualMachines", map[string]any{"e2e-full-111": 5 * time.Hour})
	f.resources("disk", "disks", map[string]any{"e2e-full-111-boot": 5 * time.Hour, "e2e-full-222-data": time.Hour})

	out, err, calls := f.run(false)
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if len(calls) != 0 {
		t.Errorf("dry run deleted %q, want nothing", calls)
	}
	for _, line := range []string{"WOULD-DELETE\tvirtual-machine\te2e-full-111\t5", "WOULD-DELETE\tdisk\te2e-full-111-boot\t5", "KEEP-YOUNG\tdisk\te2e-full-222-data\t1"} {
		if !strings.Contains(out, line) {
			t.Errorf("report is missing %q:\n%s", line, out)
		}
	}
}

func TestRegression_Sweep_AmbiguousOwnershipOrAgeIsReportedNeverDeleted(t *testing.T) {
	f := newSweepFixture(t)
	f.resources("virtual-machine", "virtualMachines", map[string]any{})
	f.resources("disk", "disks", map[string]any{
		"e2e-full-abc-data": 5 * time.Hour, // prefix matches, run id is not numeric
		"e2e-full-333-misc": 5 * time.Hour, // unknown suffix
		"e2e-full-444-boot": "not-a-time",  // unreadable age
		"e2e-full-555-boot": 5 * time.Hour, // the one clean candidate
	})

	out, err, calls := f.run(true)
	if err == nil {
		t.Fatalf("sweep.sh succeeded, want a failing exit for ambiguous resources\n%s", out)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "e2e-full-555-boot") {
		t.Errorf("delete calls = %q, want only e2e-full-555-boot", calls)
	}
	for _, line := range []string{"SKIP-AMBIGUOUS-NAME\tdisk\te2e-full-abc-data", "SKIP-AMBIGUOUS-NAME\tdisk\te2e-full-333-misc", "SKIP-AMBIGUOUS-AGE\tdisk\te2e-full-444-boot"} {
		if !strings.Contains(out, line) {
			t.Errorf("report is missing %q:\n%s", line, out)
		}
	}
}

func TestRegression_Sweep_PartialTeardownKeepsGoingAndRerunRetriesOnlyTheRest(t *testing.T) {
	f := newSweepFixture(t)
	f.resources("virtual-machine", "virtualMachines", map[string]any{"e2e-full-111": 5 * time.Hour})
	f.resources("disk", "disks", map[string]any{"e2e-full-111-boot": 5 * time.Hour, "e2e-full-111-data": 5 * time.Hour})
	f.touch("fail-delete-e2e-full-111-boot")

	out, err, calls := f.run(true)
	if err == nil {
		t.Fatalf("sweep.sh succeeded, want a failing exit when a delete fails\n%s", out)
	}
	if len(calls) != 3 {
		t.Fatalf("delete calls = %q, want the VM and both disks attempted\n%s", calls, out)
	}
	if !strings.Contains(out, "DELETE-FAILED\tdisk\te2e-full-111-boot") {
		t.Errorf("report is missing the failed delete:\n%s", out)
	}

	// Rerun after the cause is fixed: only the survivor is attempted.
	if err := os.Remove(filepath.Join(f.state, "fail-delete-e2e-full-111-boot")); err != nil {
		t.Fatal(err)
	}
	f.clearCalls()
	out, err, calls = f.run(true)
	if err != nil {
		t.Fatalf("rerun failed: %v\n%s", err, out)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "e2e-full-111-boot") {
		t.Errorf("rerun delete calls = %q, want only e2e-full-111-boot", calls)
	}

	// A third run finds nothing left.
	f.clearCalls()
	if out, err, calls = f.run(true); err != nil || len(calls) != 0 {
		t.Errorf("idempotent rerun: err=%v calls=%q, want a clean no-op\n%s", err, calls, out)
	}
}

func TestRegression_Sweep_ListFailureDeletesNothingOfThatKind(t *testing.T) {
	f := newSweepFixture(t)
	f.resources("virtual-machine", "virtualMachines", map[string]any{"e2e-full-111": 5 * time.Hour})
	f.resources("disk", "disks", map[string]any{"e2e-full-111-boot": 5 * time.Hour})
	f.touch("fail-list-virtual-machine")

	out, err, calls := f.run(true)
	if err == nil {
		t.Fatalf("sweep.sh succeeded, want a failing exit when a list fails\n%s", out)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "virtual-machine ") {
			t.Errorf("deleted a VM after its list failed: %q", c)
		}
	}
}
