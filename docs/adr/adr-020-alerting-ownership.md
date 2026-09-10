# ADR-020: Who owns alerting for the conditions in design.md §7.3?

Status: Proposed

## Question

`docs/design.md` §7.3 names six alert-worthy conditions (metric staleness beyond three publication
intervals, write failure rate above threshold, change budget exhausted, `VM_IOPS_CAP_REACHED`
sustained, a disk pinned at `max_iops` while still saturated, and a `configured_disks_unmanaged`
entry sustained beyond a few discovery cycles) but nothing defines how an operator actually receives
one of these as a page. Today every condition is visible only by scraping `/metrics` or reading logs.

Issue #59 asks for threshold/duration/severity/owner/recovery-behavior per condition and tests for
each signal's activation and clearing. Those are only answerable once one prior question is settled:
**does this repository own delivering the alert, or only producing the signal an external alerting
stack consumes?** The two answers lead to different deliverables, so implementation should not start
until the Product Lead picks one.

## Context and constraints

- `/readyz`'s binary verdict and `configured_disks_unmanaged` degradation are already specified
  (`docs/design.md` §4.1, §7.3): a runtime-degraded disk never flips `/readyz` to unready. That part
  of the semantics is settled and out of scope here — this ADR is only about what happens to the
  *signal* after it is exposed.
- The agent already exposes a raw Prometheus gauge or counter for five of the six conditions:
  `iops_autoscaler_metric_staleness_seconds`, `iops_autoscaler_change_budget_remaining` /
  `iops_autoscaler_budget_healthy`, `iops_autoscaler_vm_cap_reached`,
  `iops_autoscaler_disk_configured_unmanaged`, and `iops_autoscaler_decisions_total` (by
  action/reason, covers write-failure-rate). None of these are wired to anything beyond `/metrics`.
- "Sustained beyond N cycles/minutes" for any of the above does not require new agent-side state:
  Prometheus's own alerting-rule `for:` duration already evaluates "condition true for at least this
  long" against an existing gauge, expressed once outside the agent, and there is no plan to run any
  alternative time-series backend (`runtime.metrics.source: platform` is a metrics-collection source
  for the decision loop itself, unrelated to where alerting rules would be evaluated).
- One condition — "a disk pinned at `max_iops` while still saturated" — cannot be expressed from
  metrics that exist today: the effective ceiling (`policy.max_iops` combined with the ADR/§4.6
  headroom clamp) is never published as its own series, only the currently *applied* IOPS
  (`iops_autoscaler_disk_provisioned_iops`). Closing that specific gap is required under either
  option below and is not itself in question.
- This is an on-VM agent, not a platform-wide monitoring service: it has no existing relationship with
  any specific Alertmanager, PagerDuty, or on-call rotation, and the codebase commits to no monitoring
  stack anywhere else in `docs/platform-reference.md` or `docs/configuration.md`.

## Options

**A. Signal-only — the agent's contract stops at accurate metrics.** No alerting-rule file ships in
this repository. `docs/operations.md` (new) documents the six conditions from §7.3, the exact metric
name and label set for each, and a suggested (non-binding) threshold and `for:` duration per
condition, explicitly framed as a starting point for whatever alerting stack the deployer already
runs. Ownership of severity, paging, and escalation is the deploying team's, per fleet — the same way
`docs/configuration.md` already treats every scaling threshold as the operator's to tune, not a value
this repository asserts is correct for every deployment.

- Trade-off: fastest to ship and keeps the agent stack-agnostic (Prometheus, Datadog, or anything else
  that can scrape `/metrics`), consistent with "the likelier answer for an on-VM agent" already
  flagged in `docs/TODO.md`. Cost: a deployment that never authors rules has a real signal sitting
  unconsumed — the gap issue #59 opened with persists unless someone acts on the documentation.

**B. Ship alerting rules — the repository owns a reference rule set.** Add a versioned
`deploy/prometheus/alerts.yaml` (or equivalent) alongside the binary with a concrete `alert:`, `expr:`,
`for:`, and `severity:` for each of the six conditions, wired into release artifacts and documented as
the default an operator loads into their own Alertmanager.

- Trade-off: turns "a signal exists" into "a page fires" with no per-deployment authoring step, and
  gives the six conditions a single canonical set of thresholds to argue about instead of N divergent
  local copies. Cost: commits the project to Prometheus rule-file syntax specifically (every other
  stack is left to translate), adds a new artifact this repository must version and keep in sync with
  metric name changes, and — per §4.5's own honesty note that estimator thresholds are "unvalidated"
  until run through the `--dry-run` calibration harness (§8.5) — shipping "official" severities and
  durations before any real-world calibration risks looking more authoritative than the numbers
  actually are.

## Recommendation

Option A. The project has already chosen, for every other tunable (`docs/configuration.md`'s scaling
thresholds), to expose a mechanism and let the operator supply the number, rather than asserting one
default is right for every fleet — alerting severity and paging ownership is the same kind of
deployment-specific choice, arguably more so, since it also depends on who is on call. Prometheus's
`for:` clause already provides the sustained-duration evaluation §7.3 asks for without new agent code,
so Option A is not a lesser version of Option B — it is the same underlying signal, stopping the
repository's ownership at the boundary it already draws everywhere else.

## Decision requested

Pick Option A or Option B (or state a different split — e.g., ship rules but mark them explicitly
experimental/unvalidated). Once decided:

1. Update `docs/design.md` §7.3 with the chosen ownership model and `docs/TODO.md`'s alerting entry is
   deleted (superseded by this decision).
2. Expose the missing max-`iops`-ceiling series that both options need.
3. Under Option A: write `docs/operations.md` with the per-condition table (metric, suggested
   threshold, suggested `for:` duration, recovery behavior) and add unit tests confirming each raw
   signal activates and clears correctly (the `disk_configured_unmanaged` series already has this
   coverage in `cmd/mws-iops-autoscaler/agent_test.go`; the other five conditions do not yet).
   Under Option B: same tests, plus the rule file and its own validation (`promtool check rules` in
   CI).
4. Delete this ADR in the same change.
