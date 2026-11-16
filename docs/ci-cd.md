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
| **PR-tier e2e ("smoke")** | Self-hosted runner | `pull_request_target`, running only for same-repository branches — the origin guard lives in workflow code loaded from the base branch; see below | The agent, built once from the PR's merge commit, actually changes real disks' IOPS limits on real MWS VMs. Each `TestSmoke_*` scenario runs in its own job on its own ephemeral VM (one dependent and one independent data disk), in parallel. |
| **Merge-tier e2e ("full")** | Self-hosted runner | Push to `main`, `workflow_dispatch` | Every `TestFull_*` scenario on one shared ephemeral VM with three data disks, torn down after the run; covers device naming, live attach/detach and restart persistence. |

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

**Documentation-only PRs skip smoke (Product Lead decision, 2026-11-15).** Before `smoke`, the
GitHub-hosted `changes` job lists the PR's files through the API without checking out PR code. If
every file, including the old name of a renamed one, matches `*.md`, `docs/**` or `LICENSE`, `smoke`
is skipped with a notice. Anything else runs it: any other path (`.github/`, `e2e/`, `mirror/`), an API
error, a list shorter than the PR's `changed_files`, an empty list, or a failed `changes` job.

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
**target VM** (an ephemeral smoke or full-tier VM) is what the agent under test
actually runs on and whose disk gets scaled. They need separate credentials — see below.

**Runner processes.** The runner VM (`base-8-32`, 100 GB boot disk) hosts sixteen self-hosted runner
processes, `ci-runner` and `ci-runner-2`…`ci-runner-16`, each in its own directory under the same user
and with the same label, so up to sixteen jobs run at once. GitHub Actions gives each process one job at
a time. Each process's directory grows to about 2.5 GB (the runner, its `_work` checkout and the Go
toolchain `setup-go` installs there), so the boot disk has to grow with the process count. Smoke
scenarios share no target, so no concurrency group serializes them; the matrix's `max-parallel`
(repository variable `E2E_SMOKE_MAX_PARALLEL`, set to 10 so every scenario of a PR runs at once; 4
when unset) caps one PR's concurrent VMs. The full tier
keeps its `e2e-full` concurrency group. Running out of project quota is an expected infrastructure
failure, not a test failure: `provision.sh` exits 3, the job fails with "project quota exhausted",
nothing is retried, and teardown removes what was created.

**Network.** Test VMs get internal addresses only. The runner is in the same subnet and reaches them
over SSH at that address, so no test VM needs an external IP. Their outbound traffic (cloud-init's
package install, the agent's MWS API calls) leaves through the Egress NAT `e2e-egress` on the test
subnet, behind one external address. The runner VM keeps its own one-to-one NAT address.

**The agent on the runner.** The runner VM runs the autoscaler itself in write mode, managing its own
boot disk (`auto-discovery`, limits 1000–10000) under the runner/provisioner SA. It is a systemd unit,
`mws-iops-autoscaler.service`, running as the `iops-autoscaler` system user: binary
`/usr/local/bin/mws-iops-autoscaler`, config `/etc/mws-iops-autoscaler/config.yaml`, state
`/var/lib/mws-iops-autoscaler`, decision journal `/var/log/mws-iops-autoscaler/decisions.jsonl`, HTTP on
`127.0.0.1:9435`. To update it, build `./cmd/mws-iops-autoscaler` for `linux/amd64` from `main`, replace the
binary and `sudo systemctl restart mws-iops-autoscaler`; keep the state directory. E2E harness cleanup
matches only agents started from its own work directory on test VMs, so it never touches this one.

**Naming.** `e2e/scripts/names.sh` derives every run-scoped name, and `provision.sh`, `teardown.sh` and
the sweeper's patterns follow it: a smoke VM is `e2e-smoke-<run id>-<run attempt>-s<scenario index>`,
so a re-run never reuses an earlier attempt's VM or disks.

## Service accounts

Two separate service accounts, not one, because the runner and the agent-under-test need different
privileges:

| Service account | Attached to | Roles | Why |
|---|---|---|---|
| **runner/provisioner SA** | The runner VM (via Instance Metadata Service — see below) | `compute.editor`, `vpc.editor`, `iam.serviceAccount.admin` (resource-scoped on the agent-under-test SA) | Needs to create/delete VMs and disks (full tier) and reserve networking resources (address/NAT allocation falls under `vpc.editor`; there is no narrower address-only role in MWS's IAM catalog). Also runs the e2e test harness itself (Preflight/Cleanup, resetting the target disk), which is a VM PATCH on a VM with the agent-under-test SA attached — see the `iam.serviceAccount.admin` row below for why that needs its own separate grant. |
| **agent-under-test SA** | Every target VM, attached at creation | `compute.editor`, `iam.serviceAccount.admin` (resource-scoped on itself) | The agent calls the compute API to read its own identity and raise its disk's IOPS limit. Updating a dependent disk goes through a VM PATCH (design.md §4.7), and a VM that has this SA attached silently requires **whichever identity performs the PATCH** — not just the attached SA itself — to separately hold `iam.serviceAccount.admin` on that attached SA's resource. `.user` (the role MWS's own docs describe as sufficient for "using" an existing SA) does not work for either identity; confirmed cleanly twice, once per caller — see `docs/platform-dependencies.md` #6. Broader than intended — treat as a platform bug, not a design choice. |

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

See `docs/testing.md` for how a scenario's target state is checked and reset, what each tier
actually covers today, and how the agent process is supervised — that's test-behavior detail, not
pipeline configuration.

## Workflow configuration

`.github/workflows/e2e-pr.yml`, `.github/workflows/e2e-full.yml` and `.github/workflows/e2e-sweep.yml` read
their environment from GitHub repository variables (non-secret) and one secret:

| Name | Kind | Used by |
|---|---|---|
| `MWS_PROJECT`, `MWS_ZONE` | variable | smoke, full (sweep uses only `MWS_PROJECT`) |
| `E2E_SUBNET_ID`, `E2E_BOOT_IMAGE`, `E2E_AGENT_SA`, `E2E_SSH_PUBLIC_KEY`, `E2E_SSH_USER` | variable | smoke, full |
| `E2E_VM_TYPE` | variable | full |
| `E2E_SMOKE_VM_TYPE` | variable, optional | smoke — scenario VM type, default `compute/vmTypes/base-2-4` (2 vCPU keeps the 6000-IOPS pool the VM cap scenario relies on) |
| `E2E_SMOKE_MAX_PARALLEL` | variable, optional | smoke — concurrent scenario jobs per PR, 4 when unset; currently 10. Size it with the project quota and the runner process count |
| `E2E_SWEEP_APPLY` | variable | sweep — `true` lets the scheduled run delete; any other value reports only (see "Orphan sweeper") |
| `E2E_SSH_PRIVATE_KEY` | **secret** | smoke, full — written to a `runner.temp` file for the job's duration, removed in an `if: always()` step |

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
| `e2e-pr.yml` (smoke), per scenario job | 15m (test step, itself capped at 16m) | the longest single scenario's context, 12m (`TestSmoke_Decisions_NoFlapping`); plus a 20m provision step and an 8m teardown step | 50 |
| `e2e-full.yml` (full) | 100m | ~98m (shared fixture's 15m provisioning context, paid once, + each `TestFull_*` test's own context — `TestFull_Budget_ExhaustionPersistsAcrossRestart` at 15m for its up/down cycle and agent restart — + `TestMain`'s dedicated 8-minute teardown budget) | 112 |

`timeout-minutes` must also stay below the orphan sweeper's TTL floor (see "Orphan sweeper"). Recompute
the relevant worst-case sum before shrinking either `timeout-minutes` value, and again
whenever a `TestFull_*` test is added, or a `TestSmoke_*` scenario gets a longer context than the
longest one. Adding a smoke scenario adds a job, not time to an existing one. The full tier's margin is already thin enough
that the next addition should probably trigger the "split by subject" idea (`docs/TODO.md`) rather
than another bump.

`e2e-full.yml`'s backstop teardown step (`continue-on-error: true`, 8-minute budget) exists only for
the case where `go test`'s own `-timeout` fires and kills the test binary before
`TestMain`'s post-`m.Run()` teardown call runs (`e2e/lifecycle_test.go`). It does not cover the
job-level kill — the orphan sweeper below covers that. On an ordinary
pass or failure, `TestMain`'s own teardown has already run, so this step reports "not found" for
every resource — expected noise, not a signal. Each smoke scenario job's teardown step runs
`if: always()`, including when the run is cancelled; a resource that is already gone counts as
deleted, so a provision that failed partway tears down cleanly.

## Orphan sweeper

`.github/workflows/e2e-sweep.yml` runs `e2e/scripts/sweep.sh` daily (03:17 UTC) and on manual dispatch,
independently of the test jobs, so it still works after a job-level kill or a lost runner.

- **Ownership.** Only the names `e2e/scripts/names.sh` gives: a full-tier VM `e2e-full-<numeric run id>`
  with disks `e2e-full-<run id>-boot|data|noname|churn`, and a smoke VM
  `e2e-smoke-<run id>-<attempt>-s<index>` with disks `…-boot|indep` plus its dependent data disk, which the
  platform names `…-mza3njax` from the `data` attachment and deletes with the VM. Anything not starting
  with `e2e-full-` or `e2e-smoke-` is never considered. A name that starts with it but does not match exactly, or a missing or unparseable
  `metadata.createTime`, is reported as `SKIP-AMBIGUOUS-*`, deleted never, and fails the run.
- **Age.** `metadata.createTime` from the API, at least `ttl_hours` (default 3) old. The TTL must stay
  above the longest job that owns resources, `e2e-full.yml`'s `timeout-minutes` (112): a younger
  resource may belong to a live run, and `sweep.sh` refuses a TTL under 2 hours. A re-run of the full
  job keeps its `run_id`, so it reuses the same names and the old `createTime`; smoke names carry the
  run attempt, so a smoke re-run never does.
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

## Provisioning runbook

No Terraform — a handful of persistent resources (the runner VM and its runner processes, the two
service accounts and their role bindings, the test subnet's Egress NAT) is not enough to justify the
extra toolchain right now. Provisioned by hand via the `mws` CLI, in this order:

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
   shell out to `mws` directly, and provision.sh parses the created VM's internal address with
   `jq`; neither is part of the Go toolchain:
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
6. **Create the test subnet's Egress NAT** so test VMs without external addresses can reach the
   internet (package mirrors, the MWS API):
   ```
   mws vpc external-address create vpc/projects/<project>/externalAddresses/e2e-egress --region regions/<region>
   mws vpc egress-nat create vpc/projects/<project>/networks/<network>/egressNats/e2e-egress \
     --network <network> --internal-subnets vpc/projects/<project>/networks/<network>/subnets/<subnet> \
     --external-addresses 'ref: vpc/projects/<project>/externalAddresses/e2e-egress'
   ```
7. **Add runner processes** for parallel scenario jobs: on the runner VM, for each extra process,
   unpack the runner tarball into its own directory and register it with the same label
   (`./config.sh --unattended --url <repo URL> --token <registration token> --name ci-runner-<n>
   --labels mws-iops-autoscaler-runner`, then `sudo ./svc.sh install <user> && sudo ./svc.sh start`).
   A registration token comes from `POST /repos/<owner>/<repo>/actions/runners/registration-token`.
   Store the CI SSH private key as the `E2E_SSH_PRIVATE_KEY` secret and its public key as
   `E2E_SSH_PUBLIC_KEY`; `provision.sh` bakes the public key into every test VM's cloud-init.
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

Every smoke scenario and full-tier run creates its own VM with `ci-agent-sa` attached as part of the
workflow itself (`e2e/scripts/provision.sh`), not as a one-time step.
