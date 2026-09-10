//go:build e2e

package e2e

import "testing"

// xfail runs scenario and converts its outcome into an intentionally-red
// e2e result that never blocks a merge or release: an error is exactly what
// a known, tracked gap looks like today, so it is reported as t.Skip (still
// visible in test output, never a build failure); a nil error means the
// scenario's design-correct assertion now passes, which this package's
// convention treats as a signal the fix has already shipped — t.Fatal
// forces the xfail wrapper to be removed in the same change, rather than
// silently lingering around a scenario that no longer needs it.
//
// scenario must be a plain func() error that never touches *testing.T: Go
// gives no way to catch a t.Fatalf/t.Errorf raised from inside a test, so a
// scenario wrapped here has to report failure entirely through its return
// value (see runAgentScenarioWithDisksErr, the *testing.T-free primitive
// most such scenarios build on).
//
// issueURL names the tracked gap this scenario is red for — required so a
// skip is always traceable to a specific piece of follow-up work, never a
// bare "known failure."
func xfail(t *testing.T, issueURL string, scenario func() error) {
	t.Helper()
	if err := scenario(); err != nil {
		t.Skipf("xfail (%s): known gap, not yet fixed: %v", issueURL, err)
		return
	}
	t.Fatalf("xfail (%s): scenario succeeded — the fix has shipped; remove this xfail wrapper and let the test run for real", issueURL)
}
