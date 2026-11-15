# CI/CD

This document defines the accepted CI/CD pipeline: how changes are verified before merge, and the
infrastructure (runner, service accounts, credentials) that runs `docs/design.md` §8.3's live
end-to-end tests. It does not restate `docs/development.md`'s credential-handling rules or
reference-environment topology — read that first — and it does not describe what the e2e tests
themselves do or how that code is organized; that's `docs/testing.md`. Like `docs/development.md`, it
deliberately does not identify live resource names, project IDs, or access endpoints; those belong in
the runner's own environment, not the repository.

## Verification tiers

| Tier | Runs on | Triggers | What it proves |
|---|---|---|---|
| **Fast checks** | GitHub-hosted (`ubuntu-latest`) | Every PR, push to `main` | `gofmt`, `go vet`, `go test ./...`, `go test -race ./...`, plus the `docs/design.md` §8.2 integration suite against `internal/platformfake`. No cloud credentials involved. Implemented in `.github/workflows/ci.yml`. |
| **PR-tier e2e ("smoke")** | Self-hosted runner | `pull_request_target`, running only for same-repository branches — the origin guard lives in workflow code loaded from the base branch; see below | The agent, built from the PR's code, actually raises a real disk's IOPS limit on a real MWS VM. Reuses one persistent test VM+disk. |
| **Merge-tier e2e ("full")** | Self-hosted runner | Push to `main`, `workflow_dispatch` | Same scenario as smoke, but provisions a fresh VM+disk from scratch and tears it down — the only tier that exercises create/destroy. |

Fast checks and PR-tier e2e are **intended** to be required status checks on `main`'s branch
protection, so a PR cannot merge while either is red. Branch protection is not configured yet, for
two independent reasons: the current GitHub plan does not offer it for this private repository at
all (the runbook's step 8 records the 403 and the plan upgrade it needs), and `docs/TODO.md`
records the open question of whether PR-tier e2e should gate merges at all while PR-supplied code
runs on a credentialed self-hosted runner. As implemented, the smoke path is usable for
same-repository branches only; fork PRs are skipped by a guard evaluated in workflow code from the
base branch (see below). Withholding the SSH secret from fork `pull_request` runs would not have been
a boundary on its own: the runner's cloud credentials come from IMDS, not from a secret.

**PR-tier trust model (Product Lead decision, 2026-10-15):** live e2e runs automatically only for
pull requests from branches of this repository. `e2e-pr.yml` is triggered by `pull_request_target`,
so GitHub loads it from the base branch: a PR cannot edit away the origin guard, which a plain
`pull_request` workflow (loaded from the PR's merge commit) would allow. The `smoke` job runs only when
the PR head repository is this repository and then checks out the PR's merge ref
(`refs/pull/<n>/merge`), failing if that merge commit does not contain the current PR head (GitHub
stops updating it while the PR conflicts with the base); for fork PRs the
GitHub-hosted `fork-skipped` job prints why, and fork code is never checked out or executed. A
maintainer reviews a fork change and, if it needs live coverage, re-runs it from a branch of this
repository. Consequences: changes to `e2e-pr.yml` itself only take effect after they reach the base
branch, and a skipped `smoke` satisfies a required status check, so branch protection must be paired
with that review. The runner service account's role set is still broader than the PR tier needs
(`docs/TODO.md`).

**Public mirror (Product Lead decision, 2026-11-15):** this repository and its runner stay private.
The public repository is a mirror of `main` with cleaned history; no runner is registered to it and
it has none of the variables or the secret below. Every job that targets the self-hosted runner is
guarded by `vars.MWS_PROJECT != ''`, so in the mirror those jobs are skipped instead of queueing, and
nothing a public PR changes in `.github/` can reach the runner. A maintainer ports a public PR to a
branch here, runs the tiers, and publishes the result with the next mirror update
(`SRC=<private URL> DST=<mirror URL> mirror/publish.sh`). The script strips itself and
`docs/platform-dependencies.md` from the mirror, refuses unless `SRC` is private and `DST` is a
different, public repository, takes `mirror/replacements.txt` and `mirror/mailmap` from the `main`
it publishes (never from the checkout it runs in), rewrites commit identities through the mailmap, and fails closed
if a replaced string survives in any commit's tree, message or author/committer identity, or the
scan itself fails; `mirror/publish_test.sh` covers those refusals against local
repositories.

**Attack paths and residual risk.** Code that runs on the runner gets the runner SA's token from the
runner's IMDS with no secret, and `E2E_SSH_PRIVATE_KEY` gives it a shell on target VMs, whose own IMDS
hands out the agent-under-test SA's token. Only someone who can push a branch here can start such a
run, and today that is the maintainer alone. Before anyone else gets write access, narrow the runner's
role set or move to ephemeral per-job runners (`docs/TODO.md`).

## Runner vs. target VM — two different machines

The **runner** executes pipeline steps and issues MWS API calls; it is never the thing under test. The
**target VM** (the persistent smoke VM, or a freshly created full-tier VM) is what the agent under test
actually runs on and whose disk gets scaled. They need separate credentials — see below.

Exactly one self-hosted runner exists for now. GitHub Actions runs at most one job at a time on a given
runner, which is what makes reusing one shared persistent smoke VM safe without a locking mechanism.
The PR-tier workflow also sets an explicit `concurrency` group so this stays true even if a second
runner is added later without updating this assumption. It sets `queue: max`, so pending smoke runs
wait in order (up to 100) instead of the newest cancelling the older one. See `docs/TODO.md` for what scaling beyond one
runner would require.

## Service accounts

Two separate service accounts, not one, because the runner and the agent-under-test need different
privileges:

| Service account | Attached to | Roles | Why |
|---|---|---|---|
| **runner/provisioner SA** | The runner VM (via Instance Metadata Service — see below) | `compute.editor`, `vpc.editor`, `iam.serviceAccount.admin` (resource-scoped on the agent-under-test SA) | Needs to create/delete VMs and disks (full tier) and reserve networking resources (address/NAT allocation falls under `vpc.editor`; there is no narrower address-only role in MWS's IAM catalog). Also runs the e2e test harness itself (Preflight/Cleanup, resetting the target disk), which is a VM PATCH on a VM with the agent-under-test SA attached — see the `iam.serviceAccount.admin` row below for why that needs its own separate grant. |
| **agent-under-test SA** | Every target VM (persistent smoke VM, and each full-tier VM at creation) | `compute.editor`, `iam.serviceAccount.admin` (resource-scoped on itself) | The agent calls the compute API to read its own identity and raise its disk's IOPS limit. Updating a dependent disk goes through a VM PATCH (design.md §4.7), and a VM that has this SA attached silently requires **whichever identity performs the PATCH** — not just the attached SA itself — to separately hold `iam.serviceAccount.admin` on that attached SA's resource. `.user` (the role MWS's own docs describe as sufficient for "using" an existing SA) does not work for either identity; confirmed cleanly twice, once per caller — see `docs/platform-dependencies.md` #6. Broader than intended — treat as a platform bug, not a design choice. |

Neither account is Project Admin. `monitoring.metricExporter` is deliberately not granted to either —
the first e2e scenario verifies the IOPS change by reading the compute API directly, not platform
metrics; see `docs/TODO.md` for when that changes (fleet mode).

**Credentials never exist as a file.** Both service accounts are attached directly to their VM via
`mws compute vm update <vm> --service-account <sa>`. MWS's Instance Metadata Service (IMDS) then makes
the Go SDK auto-detect short-lived tokens on that VM with no key file, no GitHub secret, and nothing to
rotate. This is why the two-SA split matters: an IMDS-based credential is ambient on the whole machine,
so scoping what each machine can do is the only privilege boundary available (see `docs/TODO.md` for the
follow-up on narrowing the runner SA further once per-tier separation is worth the operational cost).

`monitoring.metricExporter` cannot be granted via the `mws` CLI at all — it requires a manual, one-time
grant from the MWS console. Since it is not part of the current grant, this manual step does not apply
yet.

## Guest access (SSH)

Reaching a target VM's guest OS (to install the agent binary and start `fio`) is a separate concern from
MWS API credentials — it is standard SSH, not a `mws` service-account permission. This repo already has
a convention for this in `docs/development.md`/`.env.example` (`SSH_PUBKEY_*`/`SSH_PRIVKEY_CLAUDE`): one
dedicated keypair, its public half baked into a target VM at creation via cloud-init, its private half
kept as a GitHub Actions encrypted secret and injected only for the duration of a job. CI reuses that
pattern rather than inventing per-run key rotation — see `docs/TODO.md` for that as a future hardening
step.

See `docs/testing.md` for why the shared smoke VM's state is reset the way it is, what each tier
actually covers today, and how the agent process is supervised — that's test-behavior detail, not
pipeline configuration.

## Workflow configuration

`.github/workflows/e2e-pr.yml`, `.github/workflows/e2e-full.yml` and `.github/workflows/e2e-sweep.yml` read
their environment from GitHub repository variables (non-secret) and one secret:

| Name | Kind | Used by |
|---|---|---|
| `MWS_PROJECT`, `MWS_ZONE` | variable | smoke, full (sweep uses only `MWS_PROJECT`) |
| `E2E_SMOKE_VM_NAME`, `E2E_SMOKE_DEVICE_NAME`, `E2E_SMOKE_SSH_HOST` | variable | smoke |
| `E2E_SMOKE_INDEPENDENT_DEVICE_NAME` | variable | smoke — the second, independent disk permanently attached to the same persistent VM (`E2E_SMOKE_VM_NAME`), used for the independent write path and dry-run scenarios |
| `E2E_SUBNET_ID`, `E2E_BOOT_IMAGE`, `E2E_VM_TYPE`, `E2E_AGENT_SA`, `E2E_SSH_PUBLIC_KEY` | variable | full |
| `E2E_SSH_USER` | variable | both |
| `E2E_SWEEP_APPLY` | variable | sweep — `true` lets the scheduled run delete; any other value reports only (see "Orphan sweeper") |
| `E2E_SSH_PRIVATE_KEY` | **secret** | both — written to a `runner.temp` file for the job's duration, removed in an `if: always()` step |

None of these are MWS API credentials — those come from IMDS, never from a GitHub secret (see
Service accounts above). `E2E_SSH_PRIVATE_KEY` is the one genuine secret this pipeline holds. It
grants guest-OS SSH access to the test VMs — and since every target VM has the agent-under-test SA
attached, guest access on such a VM can reach IMDS for that SA's short-lived tokens, so it
transitively grants cloud API access on that VM's attached identity. It does not grant the
runner/provisioner SA's broader project-level privileges.

## Timeout budgets

A job-level `timeout-minutes` kill has no cleanup path — GitHub Actions tears the whole job down at
that point, so no later step (not even `if: always()`) runs; the orphan sweeper below is the backstop.
Each workflow therefore sets `timeout-minutes` above the SUM of every test's own worst-case
`context.WithTimeout`, not the sum of typical durations, and sets `go test -timeout` below
`timeout-minutes` so a hung suite is killed by `go test` itself — leaving the job alive to run its
backstop teardown step — rather than by the job-level kill.

| Workflow | `go test -timeout` | Worst-case test budget | `timeout-minutes` |
|---|---|---|---|
| `e2e-pr.yml` (smoke) | 68m | ~64m (ten `TestSmoke_*` scenarios; seven at 5–6m, `TestSmoke_Decisions_VMCapReachedUnderAggregateLoad` at 10m for two disks' preflight and elevation, `TestSmoke_Decisions_NoFlapping` at 12m for its two full up/down proof cycles) | 73 |
| `e2e-full.yml` (full) | 84m | ~83m (shared fixture's 15m provisioning context, paid once, + each `TestFull_*` test's own context + `TestMain`'s dedicated 8-minute teardown budget) | 96 |

`timeout-minutes` must also stay below the orphan sweeper's TTL floor (see "Orphan sweeper"). Recompute
the relevant worst-case sum before shrinking either `timeout-minutes` value, and again
whenever a `TestSmoke_*`/`TestFull_*` test is added. The full tier's margin is already thin enough
that the next addition should probably trigger the "split by subject" idea (`docs/TODO.md`) rather
than another bump.

`e2e-full.yml`'s backstop teardown step (`continue-on-error: true`, 8-minute budget) exists only for
the case where `go test`'s own `-timeout` fires and kills the test binary before
`TestMain`'s post-`m.Run()` teardown call runs (`e2e/lifecycle_test.go`). It does not cover the
job-level kill — the orphan sweeper below covers that. On an ordinary
pass or failure, `TestMain`'s own teardown has already run, so this step reports "not found" for
every resource — expected noise, not a signal. The smoke tier has no equivalent step: the shared
target's hard-cleanup guarantee is Preflight, not teardown (`docs/testing.md`), so the next run
resets the target regardless of how the previous one ended.

## Orphan sweeper

`.github/workflows/e2e-sweep.yml` runs `e2e/scripts/sweep.sh` daily (03:17 UTC) and on manual dispatch,
independently of the test jobs, so it still works after a job-level kill or a lost runner.

- **Ownership.** Only the names `provision.sh` gives the full tier: a VM `e2e-full-<numeric run id>` and
  disks `e2e-full-<run id>-boot|data|noname|churn`. Anything not starting with `e2e-full-` is never
  considered. A name that starts with it but does not match exactly, or a missing or unparseable
  `metadata.createTime`, is reported as `SKIP-AMBIGUOUS-*`, deleted never, and fails the run.
- **Age.** `metadata.createTime` from the API, at least `ttl_hours` (default 3) old. The TTL must stay
  above `e2e-full.yml`'s `timeout-minutes` (96): a younger resource may belong to a live run, and
  `sweep.sh` refuses a TTL under 2 hours. That protection also relies on there being one runner. A
  re-run job keeps its `run_id`, so it reuses the same names and keeps the old `createTime`; with a
  second runner it could overlap a sweep (`docs/TODO.md`, runner scaling).
- **Order.** VMs first, then disks with `--purge`, as `teardown.sh` does. Every candidate is attempted and
  a failure only flips the exit status, so one stuck delete does not shield the rest.
- **Report.** One line per resource, `ACTION<TAB>kind<TAB>name<TAB>age_hours`, with `ACTION` one of
  `WOULD-DELETE`, `DELETED`, `DELETE-FAILED`, `KEEP-YOUNG`, `SKIP-AMBIGUOUS-NAME`, `SKIP-AMBIGUOUS-AGE`,
  also appended to the run's step summary.
- **Dry-run.** Dispatch with `apply` off, or leave the repository variable `E2E_SWEEP_APPLY` unset: the
  run lists what it would delete and deletes nothing. Scheduled runs delete only once `E2E_SWEEP_APPLY`
  is `true`; review a dry-run first.
- **Permissions.** No new grant: it runs on the runner VM with the runner/provisioner SA, whose
  `compute.editor` already covers listing and deleting VMs and disks (Service accounts above).
- **Recovery.** The sweep recomputes from a fresh listing each run, so rerunning is safe. After
  `DELETE-FAILED` (a disk still attached to a VM that is not ours, a quota or API error) fix the cause and
  rerun. For `SKIP-AMBIGUOUS-*` delete or rename the resource by hand; the sweeper will not. A failed
  list deletes nothing of that kind.

The persistent smoke target is not swept: it is reset by `Preflight`, not deleted.

## Provisioning runbook

No Terraform — four persistent resources (runner VM, agent-under-test SA attachment on the persistent
test VM, the two service accounts, their role bindings) is not enough to justify the extra toolchain
right now. Provisioned by hand via the `mws` CLI, in this order:

1. **Create the two service accounts:**
   ```
   mws iam service-account create ci-runner-sa
   mws iam service-account create ci-agent-sa
   ```
2. **Grant roles — console only, confirmed no CLI path exists.** `mws iam` has no role-binding
   create/set command at all (verified exhaustively: `iam service-account`, `iam role`, `resmanager`,
   and every generic resource template were checked; role bindings only exist for a couple of unrelated
   services). Everything below is done from the MWS console (Project → IAM → Service accounts):
   - `ci-runner-sa`, project-level access: `compute.editor`, `vpc.editor`.
   - `ci-agent-sa`, project-level access: `compute.editor`.
   - On **`ci-agent-sa`'s own resource page** ("Доступы к ресурсу" — not the project-level page), add
     **both** of the following as separate principals, each with `iam.serviceAccount.admin`:
     `ci-agent-sa` itself, and `ci-runner-sa`. Required because updating a dependent disk's IOPS is a VM
     PATCH, and a VM PATCH on a VM with an attached SA silently requires **whichever identity performs
     the PATCH** to separately hold `.admin` on that attached SA — the agent itself (patching its own
     VM) and the test harness running on the runner (resetting the same disk from outside) are two
     different callers, so each needs its own grant. `.user`, which MWS's own docs describe as
     sufficient for this, does not work for either. See `docs/platform-dependencies.md` #6 for the full
     reproduction (both cases confirmed cleanly, independently); this is broader than the design called
     for and should be revisited once the platform team responds.
3. **Create the runner VM** (Ubuntu 24.04, modest size — it only runs Go builds and issues API calls, it
   is not under load) and attach `ci-runner-sa`:
   ```
   mws compute vm update <runner-vm> --service-account ci-runner-sa
   ```
4. **Install and register the GitHub Actions self-hosted runner** on that VM, following GitHub's own
   runner-registration flow (Settings → Actions → Runners → New self-hosted runner), labeling it so
   workflows can target it specifically (e.g. `mws-iops-autoscaler-runner`).
5. **Install the `mws` CLI and `jq` on the runner VM** — `e2e/scripts/provision.sh`/`teardown.sh`
   (the full-tier test) shell out to `mws` directly, and provision.sh parses the created VM's
   external IP with `jq`; neither is part of the Go toolchain:
   ```
   curl -sSL https://storage.mwsapis.ru/mws-cli/install.sh | bash
   sudo ln -sf ~/.local/bin/mws /usr/local/bin/mws   # GitHub Actions runs non-interactive
                                                        # shells that don't source ~/.bashrc
   sudo apt-get install -y jq
   mws init --vm-sa --project <project> --zone <zone> --no-input --force
   ```
   `--vm-sa` authenticates via the runner's own attached service account through IMDS — same "no key
   file" model as the Go SDK, confirmed live (`mws profile list` shows the resulting profile with no
   `SUBJECT` shown, since it's resolved fresh from IMDS on each call rather than stored).
6. **Create the persistent PR-tier test VM+disks**, per `docs/development.md`'s reference topology
   (Ubuntu 24.04, ≥2 vCPU, NBS-PL2 boot disk + one dependent data disk + one independent data disk,
   all at 1000 IOPS — matching `docs/design.md` §8.3's `iops-test-vm`), and attach `ci-agent-sa`.
   The dependent disk is defined inline in the VM's own `--storage-disks` at creation; the independent
   one is created standalone and attached afterward. `mws compute vm update --storage-disks` **resends
   the VM's whole disk list** (`docs/platform-reference.md`) — omitting an already-attached disk from
   it detaches that disk, so the attach step must re-list the existing boot and dependent-data entries
   alongside the new one, not just add it:
   ```
   mws compute disk create <independent-data-disk> \
     --zone <zone> --disk-type compute/diskTypes/nbs-pl2 --size 20GB --iops 1000

   mws compute vm update <test-vm> --service-account ci-agent-sa \
     --storage-disks '<existing boot disk entry, unchanged>' \
     --storage-disks '<existing dependent data disk entry, unchanged>' \
     --storage-disks 'name: indep, boot: false, deviceName: <independent-device-name>, disk: {ref: "compute/projects/<project>/disks/<independent-data-disk>"}'
   ```
   The VM's cloud-init must install `fio` and configure passwordless sudo for the CI SSH user —
   `DriveLoad` (e2e/scenario.go) runs fio under sudo because the guest user does not own the raw
   block device; provision.sh gives the full tier's ephemeral VMs the same user-data, so this step
   mirrors it for the persistent one.
7. **Bake the CI SSH public key** into the persistent test VM's cloud-init at creation (see Guest access
   above); store the matching private key as a GitHub Actions encrypted secret.
8. **Configure branch protection on `main`** requiring the `verify` (fast-checks) and `smoke`
   (PR-tier e2e) jobs as required status checks — blocked by two independent things, both of which
   must be resolved first:
   - **Plan limitation:** branch protection is not available to this private repository on the
     current GitHub plan — the API answers 403, "Upgrade to GitHub Pro or make this repository
     public to enable this feature" (verified 2026-09-06). Upgrade to a plan that supports branch
     protection. Do **not** make this repository public to obtain it: publication goes through the
     public mirror (see the PR-tier trust model above), and this repository keeps the runner.
   - **Trust model:** decided (same-repository branches only, see the PR-tier trust model above).
   Once the plan allows it, configure `verify` and `smoke` as required status checks.
9. **Confirm GitHub's fork-approval requirement is active** for workflow runs from outside contributors
   (default: first-time contributors need approval). Approval only permits execution. `e2e-pr.yml` is
   `pull_request_target` and skips fork PRs by design, so an approved fork PR never reaches the
   credentialed runner — see the PR-tier trust model above. Never register the runner, or copy the
   variables and the secret, to the public mirror.

Every full-tier run additionally creates and attaches `ci-agent-sa` to its own freshly created VM as part
of the workflow itself (not a one-time step) — see step 3 above's per-run analogue in
`.github/workflows/e2e-full.yml`.
