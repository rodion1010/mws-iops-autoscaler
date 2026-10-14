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
- **Fork-PR handling needs a decision before the repo goes public.** GitHub's default only requires
  approval for a contributor's *first* PR; afterward their workflows run unattended on the self-hosted
  runner. Approval alone never makes fork smoke work, though: the job needs the `E2E_SSH_PRIVATE_KEY`
  secret, which is not sent to public-fork `pull_request` workflows at all (private-fork secret
  sharing is a separate, explicit repository/organization setting), so an approved fork PR's smoke run
  just fails on the missing key. Before going public, either stop triggering the PR-tier e2e job from
  `pull_request` entirely (label-gated or maintainer-dispatched instead) or build the secret and
  runner-isolation model from the trust-boundary item below.
- **Runner SA privilege is broader than the PR-tier smoke test needs.** The runner's SA carries
  `compute.editor` + `vpc.editor` so the same runner can also drive the merge-tier full
  create/destroy test; any code that reaches the runner (post fork-approval) can use all of it, not just
  what a smoke run touches. Revisit splitting into per-tier runners/SAs, or dropping `vpc.editor` if the
  network/address setup turns out not to need per-run changes, once it's clear this matters in practice.
- **CI-dedicated SSH key is long-lived, not rotated per run.** Reused this repo's existing
  `SSH_PRIVKEY_CLAUDE`-style pattern (one keypair, public key baked into target VMs at creation, private
  key as a GitHub secret) for speed. A per-run ephemeral keypair (generated in `$RUNNER_TEMP`, public key
  injected only for that run, removed after) would be tighter; not built now.
- **No orphan sweeper for merge-tier ephemeral resources.** The merge-tier full test tags what it creates
  and tears down on completion, but nothing independently sweeps resources left behind by a crashed run,
  a cancelled pipeline, or a runner that lost its connection mid-test. Needs a periodic job checking for
  stale run-id-tagged resources past some TTL.
- **No automated drift check for the hand-provisioned persistent resources.** Runner VM, agent-under-test
  SA bindings, the persistent test VM/disk's baseline IOPS, and the GitHub fork-approval setting are all
  provisioned by hand per `docs/ci-cd.md`'s runbook, with no idempotent "verify environment matches the
  runbook" check. Worth a checklist/script once drift actually causes a confusing failure.
- **Branch protection on `main` is not configured.** `docs/ci-cd.md` describes fast checks and PR-tier
  e2e as intended required status checks, but nothing enforces that yet — a red check does not block a
  merge today. Two independent blockers: (a) the current GitHub plan does not offer branch protection
  for this private repository at all — the API answers 403, "Upgrade to GitHub Pro or make this
  repository public to enable this feature" — so it needs a plan upgrade, and *not* making the repo
  public (that must wait for the trust-boundary decision below); and (b) the trust-boundary question
  below: making PR-tier e2e *required* is what turns "PR code runs on the credentialed runner" from
  an option into a merge prerequisite. Order: upgrade the plan, resolve the trust decision, then
  configure `verify` and `smoke` as required checks.
- **Trust boundary: PR-supplied code runs on a credentialed self-hosted runner.** `e2e-pr.yml` checks
  out and executes the PR's own code on the persistent runner, whose attached SA is ambient via IMDS,
  and whose SSH key reaches target VMs (which themselves expose an SA through their own IMDS — so
  guest access is not the "SSH only, no cloud API" boundary it might look like). Known critical risk,
  accepted for now with two trusted committers on a private repo; needs a real decision before the
  repo goes public or before PR-tier e2e becomes a required check. As implemented, the smoke path is
  usable for same-repository collaborator branches only: the job requires the `E2E_SSH_PRIVATE_KEY`
  secret, which GitHub never sends to public-fork `pull_request` workflows (private-fork secret
  sharing is a separate, explicit repository/organization setting) — fork-PR approval permits
  execution but does not deliver the secret, so an approved fork PR's smoke run fails on the missing
  key. Fork-PR smoke is unresolved/unsupported until a safe secret and runner-isolation model is
  chosen. Options: keep it same-repo-only and informational, gate it behind a maintainer-applied
  label, or move to ephemeral per-job runners with per-run credentials so no long-lived credentialed
  machine executes untrusted code.
- **One of `docs/design.md` §8.3's ten e2e scenarios is not implemented: VM cap detection.** See
  `docs/testing.md`'s coverage table for the honest per-scenario status of the other nine — scale-down
  after quiet and no-flapping now have real smoke-tier coverage, built against an `e2e_scaledown`-tagged
  test binary (`allowScaleDown = true`) separate from the production build.

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

### What's left to actually flip scale-down on in production

PRD Phase 3's exit criterion is "soak and chaos tests green with scale-down enabled, `local` mode"
(design.md M9). The scale-down decision logic, write path, and now smoke-tier e2e coverage
(`TestSmoke_Decisions_ScaleDownAfterQuiet`, `TestSmoke_Decisions_NoFlapping`) are all done — what
remains is three separate things, none of them more code in the sense the scenarios above are:

- **Soak and chaos don't exist as a CI tier for any scenario today**, not just scale-down (design.md
  §8.4: 24h diurnal-load soak, 20%-fault-injection chaos, both local-mode). A 24h job cannot fit either
  existing workflow's timeout model (`docs/ci-cd.md`'s "Timeout budgets") and would occupy this
  project's one self-hosted runner for its entire duration, blocking every PR/merge e2e run unless a
  second runner exists first (the item above) or it is scheduled off-hours. Chaos additionally needs a
  fault-injection layer nothing in `internal/platformfake` or `e2e/` has today. This almost certainly
  needs its own decision (new runner vs. scheduled `workflow_dispatch` job vs. explicitly accepting the
  gap for longer) before anyone builds it — a candidate for an ADR rather than a quiet bump.
- **The calibration harness (design.md §8.5) is a manual process, not a tool.** It reuses `--dry-run`
  and the decision journal to build a corpus of (decision-that-would-have-fired, what-the-load-did-next)
  pairs against real Phase 1–2 telemetry, then tunes the default thresholds against it. Nothing
  automates the corpus collection or the tuning step; both need real production-like traffic to exist
  against, which in turn needs scale-up (and, per Phase 2, platform-metrics comparison mode) to have
  actually been running live for a while first.
- **Flipping `cmd/mws-iops-autoscaler/allowscaledown_prod.go`'s `false` to `true` is the literal last
  step**, deliberately kept to one file so Phase 3 really does "flip this one thing and nothing else"
  — but it should only happen once the two items above have actually been satisfied, not as a
  consequence of this file's own existence.
