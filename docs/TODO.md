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
- **Seven of `docs/design.md` §8.3's ten e2e scenarios are not implemented.** Three are covered today:
  scale-up on saturation, plus the dependent and independent write paths — each tier exercises its own
  real write path with the disk's class asserted per tier (`AssertDiskClass`), not separate test cases
  of their own. See `docs/testing.md`'s coverage table for the honest per-scenario status. Two of the
  missing seven (scale-down, no-flapping) are blocked on Phase 3 shipping scale-down at all; the rest
  are just unwritten.
