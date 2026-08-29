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

### CI/CD and E2E test infrastructure on self-hosted GitLab Runners

There is currently no CI/CD infrastructure at all — no automated build, no automated unit/integration
run, and no way to exercise the agent against a real MWS VM and disk. We need one, and testing this
project honestly eventually means writing real IOPS values to a real disk and watching the platform
react, which local unit tests can't cover.

**Constraint, already decided:** not GitHub Actions. Pipelines run on GitLab Runner(s) hosted on our
own MWS Cloud Platform infrastructure. Everything else below is open.

**Reference, not a blueprint:** `github.com/user578/mws_webinar_26-04` has MWS Terraform for
VPC/subnet, Kubernetes, GitLab + Runner deployment, and prep scripts. Worth reusing provider
configuration and individual resource examples from; its architecture (Kubernetes in particular) is
not something to copy by default — see the open question below.

**Two infrastructures, different lifetimes:**

- **Persistent CI infrastructure** — exists all the time, runs pipelines. Candidate pieces: the
  runner(s) themselves, a CI service account, its IAM grant, network (if the runner needs one beyond
  what MWS gives every VM), somewhere to keep Terraform state, somewhere to keep CI/CD secrets, maybe
  a container registry / artifact store.
- **Ephemeral E2E infrastructure** — created per pipeline run, destroyed after. At minimum: a test VM,
  a disk attached to it with specific parameters, the agent installed/running on it, a controlled I/O
  load (fio), a wait for the agent to react, verification that the disk's IOPS actually changed via
  the API, log/metric/result collection, then teardown.

**Open — not to be decided in passing:**

- **Kubernetes or plain VM(s) for the runner.** The reference repo uses Kubernetes; that is not a
  reason to default to it here. Prefer whatever is minimal in cost and operational complexity for
  what we actually need — likely one or a few VMs running `gitlab-runner`, unless a concrete reason
  for Kubernetes shows up.
- **Terraform layout.** A tentative split — `infra/ci/` (persistent), `infra/e2e/` (ephemeral),
  reusable modules for VM/disk/network — is a reasonable starting guess, not a decision. Design it
  when the work is picked up, not before.
- **IAM for the CI service account.** Needs to be scoped to what E2E actually requires — create/update
  IAM: create/delete VMs, create/change/delete disks, network operations if needed, read platform
  metrics, whatever else the first scenario turns out to need. Not Project Admin. How credentials
  reach the runner and Terraform safely (not hardcoded in the repo) is part of the same question.
- **E2E isolation.** Tests must be independently runnable and parallelizable: unique resource names
  per pipeline/job, `pipeline_id`/`job_id` tags on everything created, a separate Terraform state per
  test environment, no dependency on a pre-existing, slowly-drifting "the test VM."
- **Cleanup as architecture, not a shell script bolted on afterward.** Must reliably run when the test
  fails, the agent crashes, the pipeline is cancelled, it times out, or the runner loses its
  connection mid-test. Wants both a primary teardown path and a secondary sweep for orphaned
  resources — e.g. a TTL / pipeline-id tag every ephemeral resource carries, checked by a periodic
  cleanup job independent of any single pipeline run.

**First E2E scenario (proof of concept for the whole vertical path, not just the infra):** pipeline
creates a VM and disk → installs the agent → runs `fio` with a load that should trigger scale-up →
agent decides to raise IOPS → test confirms via the API that the disk's IOPS actually changed → agent
and `fio` logs/artifacts are saved → Terraform destroys the environment. Later scenarios (scale-down,
dry-run, platform vs. guest metrics, other disk types, API-error injection, fleet mode) should be able
to reuse the same infrastructure without redesigning it.

**Definition of done, per the original ask:** CI infra is code and reproducibly deployable; the
runner can run this project's pipeline; no hardcoded credentials; the pipeline can create a VM and
disk in MWS; the agent and a test load can run on that VM; the first E2E scenario automatically
verifies the agent's behavior; artifacts and logs survive the pipeline; created infrastructure is
reliably deleted; there is a guard against orphaned resources; and there's documentation for both
standing up the CI infrastructure and reproducing the E2E environment locally.
