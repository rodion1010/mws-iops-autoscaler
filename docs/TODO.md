# TODO — parking lot

Ideas and problems noticed while working on something else. This file exists so a good thought is
not lost and does not derail whatever is currently in progress.

## How to use this file

**Do not pick work from here.** Items are recorded, not scheduled. An item moves out of this file
only when we explicitly decide to work on it — not because it looks small, not because it is adjacent
to the current task, not because the file was open.

Adding to it is always fine. Acting on it requires a decision.

When an item is picked up: state which one, do the work, delete the entry in the same change.

Format: `### <short title>` + what the problem is, why it matters, and any options already considered.
Leave the decision open unless it is genuinely settled.

---

## Open items

### CI/CD hardening deferred from the initial GitHub Actions build

The live e2e CI/CD design (`docs/ci-cd.md`) deliberately ships a minimal-but-safe first version.
These were identified during design/review but explicitly deferred, not solved:

- **`monitoring.metricExporter` on the agent-under-test service account.** Not granted yet because the
  first e2e scenario verifies the IOPS change via the compute API, not platform metrics. Add it (console-only
  grant, per `docs/platform-reference.md`) when fleet mode's `metrics.source: platform` needs live e2e
  coverage.
- **Runner SA privilege is broader than the PR-tier smoke test needs.** The runner's SA carries
  `compute.editor` + `vpc.editor` so the same runner can also drive the merge-tier full
  create/destroy test; any code that reaches the runner (post fork-approval) can use all of it, not just
  what a smoke run touches. Revisit splitting into per-tier runners/SAs, or dropping `vpc.editor` if the
  network/address setup turns out not to need per-run changes, once it's clear this matters in practice.
- **CI-dedicated SSH key is long-lived, not rotated per run.** Reused this repo's existing
  `SSH_PRIVKEY_CLAUDE`-style pattern (one keypair, public key baked into target VMs at creation, private
  key as a GitHub secret) for speed. A per-run ephemeral keypair (generated in `$RUNNER_TEMP`, public key
  injected only for that run, removed after) would be tighter; not built now.
- **No automated drift check for the hand-provisioned persistent resources.** Runner VM, agent-under-test
  SA bindings, the persistent test VM/disk's baseline IOPS, and the GitHub fork-approval setting are all
  provisioned by hand per `docs/ci-cd.md`'s runbook, with no idempotent "verify environment matches the
  runbook" check. Worth a checklist/script once drift actually causes a confusing failure.
- **Branch protection on `main` is not configured.** `docs/ci-cd.md` describes fast checks and PR-tier
  e2e as intended required status checks, but nothing enforces that yet — a red check does not block a
  merge today. Two independent blockers: (a) the current GitHub plan does not offer branch protection
  for this private repository at all — the API answers 403, "Upgrade to GitHub Pro or make this
  repository public to enable this feature" — so it needs a plan upgrade, and *not* making the repo
  public (until the runner's role set is narrowed); and (b) the PR-tier trust model is now decided
  (same-repository branches only), but a skipped fork `smoke` satisfies a required check, so fork PRs
  need maintainer review. Order: upgrade the plan or publish, then configure `verify` and `smoke` as
  required checks.
- **Residual risk of the PR-tier trust model: PR-supplied code still runs on a credentialed
  self-hosted runner.** Live e2e is limited to branches of this repository (see `docs/ci-cd.md`'s
  PR-tier trust model), which removes fork code but not the ambient privilege: `e2e-pr.yml` executes
  the branch's own code on the persistent runner, whose attached SA is ambient via IMDS, and whose SSH
  key reaches target VMs (which themselves expose an SA through their own IMDS). Remaining work:
  narrow the runner's role set (next item), and consider ephemeral per-job runners if external
  committers are ever added.
- **One of `docs/design.md` §8.3's ten e2e scenarios is not implemented: VM cap detection.** See
  `docs/testing.md`'s coverage table for the honest per-scenario status of the other nine.

### Alerting: what the agent should page a human about, and how

`docs/design.md` §7.3 lists alert-worthy conditions (metric staleness beyond three publication
intervals, write failure rate above threshold, budget exhaustion, `/readyz` false) but nothing
defines how an operator actually receives them. Today every one of these is visible only by scraping
`/metrics` or reading logs — the agent has no notion of an alert, and the repository ships no
alerting rules.

This became concrete while planning e2e discovery scenarios: several of them ("a `selected`
disk disappeared", "a `selected` disk is not attached yet") were cases where the *right* product
behaviour was "keep autoscaling the other disks and raise a loud, addressable signal" rather than
"stop acting" — now implemented (docs/design.md §4.1, docs/configuration.md §4.2): a runtime mismatch
degrades only the affected disk, surfaced via `/readyz`'s `configured_disks_unmanaged` and the
`iops_autoscaler_disk_configured_unmanaged{device_name,reason}` gauge. That signal existing is not the
same as it reaching anyone, though, which is exactly what stays undefined.

Open question — does the agent own alerting at all, or only expose signals for a platform-side
alerting stack, and if the latter, do we ship recommended Prometheus alerting rules alongside the
binary — is now a pending decision request: [`docs/adr/adr-020-alerting-ownership.md`](adr/adr-020-alerting-ownership.md)
(issue #59).

### Config hot-reload

The agent reads its config once, in `LoadAndValidate` at startup: there is no SIGHUP handler and no
file watch, so every configuration change needs a process restart. Rediscovery already picks up newly
attached *disks* without a restart (`docs/configuration.md` §4.2), which makes the asymmetry easy to
miss — what cannot change at runtime is the operator's *intent*, not the inventory.

Noticed while planning e2e discovery scenarios: moving a disk in or out of `disks.exclude`, or adding
an already-attached disk to `disks.selected`, are ordinary operational actions, and today each costs a
restart — which also throws away the sampling windows and in-memory cooldowns the agent needs before
it can act again. Those scenarios are written against a restart for now; the same tests would cover a
reload mechanism unchanged, only swapping the restart step.

Open questions: SIGHUP or a file watch (a watch also reloads on a half-written file, so it needs the
usual atomic-rename discipline); what happens to sampling windows and cooldowns of disks that stay in
scope across a reload; whether an invalid new config is rejected while the old one keeps running, and
how that state is surfaced.

### Second CI runner, split by tier, and a second persistent smoke VM

Today one self-hosted runner serves both e2e tiers, so a merge-tier full run (~20 min) blocks every
PR's smoke run behind it, and all smoke runs are additionally serialized on one shared persistent VM
(`concurrency: e2e-smoke-vm`) regardless of runner count.

Two independent moves, worth separating:
- A second runner dedicated to one tier (e.g. runner A = PR smoke only, runner B = merge full only)
  removes the tier-vs-tier queueing with no new cloud resources.
- A second persistent smoke VM would let PR smoke runs parallelize across PRs, but only pays off
  once the PR-tier suite is large enough that queueing on a single VM is the actual bottleneck —
  decide after the real per-suite timing measurement from the growing e2e scenario set.

### Validating scale-down's defaults against real traffic

Scale-down is live (no capability gate — decided while the agent still has no real users, so there
was nothing to protect by staging the rollout further). Its decision logic, write path, and
smoke-tier e2e coverage (`TestSmoke_Decisions_ScaleDownAfterQuiet`, `TestSmoke_Decisions_NoFlapping`)
are done. Two things from the original Phase 3 plan are still worth doing, now as ordinary hardening
rather than a launch gate:

- **Soak and chaos don't exist as a CI tier for any scenario today** (design.md §8.4: 24h diurnal-load
  soak, 20%-fault-injection chaos, both local-mode). A 24h job cannot fit either existing workflow's
  timeout model (`docs/ci-cd.md`'s "Timeout budgets") and would occupy this project's one self-hosted
  runner for its entire duration, blocking every PR/merge e2e run unless a second runner exists first
  (the item above) or it is scheduled off-hours. Chaos additionally needs a fault-injection layer
  nothing in `internal/platformfake` or `e2e/` has today. This needs its own decision (new runner vs.
  scheduled `workflow_dispatch` job vs. accepting the gap) before anyone builds it — a candidate for an
  ADR rather than a quiet bump.
- **The calibration harness (design.md §8.5) is a manual process, not a tool.** It reuses `--dry-run`
  and the decision journal to build a corpus of (decision-that-would-have-fired, what-the-load-did-next)
  pairs against real traffic, then tunes the default thresholds against it. Nothing automates the
  corpus collection or the tuning step. No default in `docs/configuration.md` should be treated as
  validated until it has survived this — true for scale-down's defaults now exactly as it already was
  for scale-up's.
- **The durable 24h change budget's own acceptance criterion (issue #51) has no live coverage.**
  `TestSmoke_Decisions_NoFlapping` proves two real up/down cycles and bounds alternation count, but it
  runs against a budget the smoke harness deliberately resets to empty every run (`docs/testing.md`
  "Why preflight matters more than cleanup") — nothing live proves the budget actually suppresses the
  next otherwise-valid change once exhausted, or that the suppression survives an agent restart.
  Doesn't fit inside the smoke tier's shared-VM, reset-per-run model; needs its own full-tier scenario
  that drives real actuations toward `max_changes_per_24h`, confirms `budget_exhausted`, restarts the
  agent, and confirms the count (not just the state file) persisted.
