//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// budgetScenarioMaxChanges is small enough for one scale-up/scale-down cycle
// to exhaust it.
const budgetScenarioMaxChanges = 2

// withChangeBudget adds a policy.advanced.budget override to a rendered agent
// config, failing if the advanced block it hangs off is missing.
func withChangeBudget(configYAML string, maxChanges int) (string, error) {
	const anchor = "\n  advanced:\n"
	if strings.Count(configYAML, anchor) != 1 {
		return "", fmt.Errorf("e2e: add change budget: want exactly one policy.advanced block in the rendered config, got:\n%s", configYAML)
	}
	return strings.Replace(configYAML, anchor, fmt.Sprintf("%s    budget: {max_changes_per_24h: %d}\n", anchor, maxChanges), 1), nil
}

// countActuations counts records that reserved a budget slot.
func countActuations(recs []journalRecord) int {
	n := 0
	for _, rec := range recs {
		if rec.Action == "scale_up" || rec.Action == "scale_down" {
			n++
		}
	}
	return n
}

// isBudgetExhaustedRecord matches a change suppressed only by the budget. The
// decider checks the budget after cooldown and guardrails, so this reason
// implies the change was otherwise valid; an explicit budget_remaining <= 0 shows the
// count itself, not just a reason string, reached the cap.
func isBudgetExhaustedRecord(rec journalRecord) bool {
	return rec.Action == "no_action" && rec.Reason == "budget_exhausted" &&
		rec.BudgetRemaining != nil && *rec.BudgetRemaining <= 0
}

// TestFull_Budget_ExhaustionPersistsAcrossRestart drives real actuations up to
// a small max_changes_per_24h, confirms the next otherwise-valid change is
// suppressed without touching the disk, then restarts the agent on the same
// state dir and confirms the suppression survives. Cooldowns are in memory
// and lost on restart, so only the durable budget can still suppress it.
func TestFull_Budget_ExhaustionPersistsAcrossRestart(t *testing.T) {
	fix, ok := ensureFullFixture(t)
	if !ok {
		return
	}
	target := fix.dataTarget()
	project, err := Project()
	if err != nil {
		SkipOrFail(t, err)
		return
	}

	// Worst case: ~1.5m preflight + ~1m build/install + 4m up/down cycle +
	// 2m to budget_exhausted + 45s no-change + ~15s restart + 3m to
	// budget_exhausted after cold start + 45s no-change, with margin.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client, err := NewComputeClient(ctx, project)
	if err != nil {
		t.Fatalf("build compute client: %v", err)
	}
	// Preflight also wipes the state dir an earlier full-tier test left on
	// the shared VM, so the budget starts empty.
	if err := Preflight(ctx, client, target); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	// Unlike other full-tier tests this one restores the baseline, on
	// failure too: the cycle may leave the disk off baseline.
	defer func() {
		cleanupCtx, cleanupCancel := recoveryContext()
		defer cleanupCancel()
		if err := Cleanup(cleanupCtx, client, target); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	configYAML, err := withChangeBudget(agentConfigYAML(target), budgetScenarioMaxChanges)
	if err != nil {
		t.Fatalf("%v", err)
	}
	binPath, cleanupBin, err := BuildAgentBinary(ctx, "..")
	if err != nil {
		t.Fatalf("build agent binary: %v", err)
	}
	defer cleanupBin()
	if err := InstallAgent(ctx, target, binPath); err != nil {
		t.Fatalf("install agent: %v", err)
	}
	if _, err := target.Run(ctx, "cat > "+remoteWorkDir+"/config.yaml <<'EOF'\n"+configYAML+"EOF"); err != nil {
		t.Fatalf("install budget config: %v", err)
	}

	fail := func(format string, args ...any) {
		t.Helper()
		diagCtx, diagCancel := recoveryContext()
		defer diagCancel()
		t.Fatalf("%s\njournal:\n%s\nagent log:\n%s", fmt.Sprintf(format, args...), FetchJournal(diagCtx, target), FetchAgentLog(diagCtx, target))
	}

	if err := StartAgent(ctx, target, false); err != nil {
		t.Fatalf("start agent: %v", err)
	}

	// Two real actuations, one per direction, spend the whole budget.
	up, down, err := proveUpDownCycle(ctx, client, target, target.BaselineIOPS)
	if err != nil {
		fail("spend the budget: %v", err)
	}
	t.Logf("spent the budget: %d -> %d -> %d", target.BaselineIOPS, up, down)
	settled := target
	settled.BaselineIOPS = down

	// The next change would be this load's scale-up from the cycle's floor.
	if err := DriveLoad(ctx, target); err != nil {
		fail("drive load: %v", err)
	}
	assertSuppressed := func(phase string, timeout time.Duration, wantActuations int) {
		t.Helper()
		rec, err := WaitForJournalRecord(ctx, target, timeout, 5*time.Second, isBudgetExhaustedRecord)
		if err != nil {
			fail("%s: wait for budget_exhausted: %v", phase, err)
		}
		t.Logf("%s: suppressed at current=%d budget_remaining=%d", phase, rec.CurrentIOPS, *rec.BudgetRemaining)
		if err := AssertNoIOPSChange(ctx, client, settled, 45*time.Second, 5*time.Second); err != nil {
			fail("%s: budget_exhausted must leave the disk alone: %v", phase, err)
		}
		recs, err := journalRecords(ctx, target)
		if err != nil {
			fail("%s: read journal: %v", phase, err)
		}
		if got := countActuations(recs); got != wantActuations {
			fail("%s: journal holds %d scale_up/scale_down records, want %d", phase, got, wantActuations)
		}
	}

	assertSuppressed("before restart", 2*time.Minute, budgetScenarioMaxChanges)

	// Restart on the same state dir. fio keeps running so the restarted
	// agent sees the same saturation; the old journal is moved aside because
	// the agent appends to it and its records would match immediately.
	if err := stopStaleAgent(ctx, target, defaultAgentStopTiming); err != nil {
		fail("stop agent for restart: %v", err)
	}
	if _, err := target.Run(ctx, "mv "+remoteWorkDir+"/journal.jsonl "+remoteWorkDir+"/journal-before-restart.jsonl"); err != nil {
		fail("move journal aside: %v", err)
	}
	if err := StartAgent(ctx, target, false); err != nil {
		fail("restart agent: %v", err)
	}

	assertSuppressed("after restart", 3*time.Minute, 0)
}
