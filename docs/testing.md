# Testing

How `docs/design.md` §8's three automated tiers (unit, integration, e2e) are actually implemented in
this repository: what each one covers, how to run it, and how the test code itself is organized. This
document does not define the accepted test *strategy* — that's `docs/design.md` §8, read that first —
it describes the concrete implementation of §8.2 and §8.3. It also does not describe the CI/CD pipeline
that runs these automatically; that's `docs/ci-cd.md`.

## The three tiers, and where their code lives

| Tier | Design.md | Build tag | Lives in | Needs real infrastructure? |
|---|---|---|---|---|
| Unit | §8.1 | none | `internal/*/*_test.go` | No |
| Integration | §8.2 | `integration` | `cmd/mws-iops-autoscaler/integration_test.go`, `internal/platformfake/` | No — fakes the MWS API |
| End-to-end (live) | §8.3 | `e2e` | `e2e/` | Yes — a real MWS VM and disk |

```bash
go test ./...                              # unit only (default, no tag)
go test -race -tags integration ./...      # + integration, against the in-process fake
go test -count=1 -tags e2e ./e2e/... -run 'TestSmoke|TestRegression' # + live e2e (needs docs/ci-cd.md's environment)
```

`-count=1` is mandatory for the live tier, not politeness: the e2e package does not import
`cmd/mws-iops-autoscaler` — `BuildAgentBinary` builds the agent binary inside the test — so an
agent-code-only change leaves the e2e package's test inputs unchanged and Go's test cache would
replay a stale pass without building the changed agent or touching the VM (confirmed live: a smoke
run "completed" instantly with `(cached)` after an agent-code-only change). The CI workflows pass it
for the same reason.

## Integration tests (§8.2) — `internal/platformfake`

`internal/platformfake` is an in-process fake of the MWS Compute API and instance metadata service.
`cmd/mws-iops-autoscaler/integration_test.go` drives the real `Agent` against it, so these tests
exercise the actual SDK-to-platform boundary (real HTTP status codes, real request/response shapes)
without any credentials or network access.

**It lives in `internal/`, not `e2e/`, on purpose** — the two are complementary, not related by
directory nesting:
- `internal/platformfake` exists so integration tests *don't* need real infrastructure — it fakes the
  platform.
- `e2e/` exists specifically to test *against* real infrastructure — it would defeat the point to fake
  anything there.

It sits under `internal/` because `cmd/mws-iops-autoscaler/integration_test.go` (a different package)
imports it, and Go's `internal/` visibility rule is exactly "importable by anything in this module,
nothing outside it" — the standard place for shared test-support code that isn't itself a `_test.go`
file. See its own package doc for what each fixture and injection helper does.

## End-to-end tests (§8.3) — `e2e/`

Two tiers, sharing one set of scenario steps, differing only in where the target VM comes from:

| Test | Tier | Target VM |
|---|---|---|
| `TestSmoke_ScaleUpOnPersistentVM` | PR-tier ("smoke") | Reuses one persistent VM+disk |
| `TestFull_ProvisionScaleUpTeardown` | Merge-tier ("full") | Provisions a fresh VM+disk, tears it down after |

**File map** — the same one in `e2e/config.go`'s package doc, expanded here:

| File | What it's for |
|---|---|
| `config.go` | `Target` (one VM+disk pair under test) and environment-variable loading (`SmokeTarget`, `Project`, `Zone`, `SkipOrFail`). |
| `ssh.go` | Guest-OS access primitives (`Run`, `RunBackground`, `Copy`) over the system `ssh`/`scp` binaries — not a Go SSH library, since this is harness-only code never shipped in the production binary. |
| `reset.go` | Builds the compute-API client (`NewComputeClient`) and discovers/verifies/resets a disk (`AssertDiskClass`, `ResetDiskToBaseline`, `CurrentIOPS`) — shared by both tiers' preflight and cleanup. |
| `scenario.go` | The actual scenario: `Preflight`, `InstallAgent`, `StartAgent`, `StopAgent`, `DriveLoad`, `WaitForScaleUp`. **This is where "what the test does" lives** — both tiers call these in the same order. |
| `build.go` | Cross-compiles the agent binary for the target's guest OS (linux/amd64) regardless of what the runner itself is. |
| `smoke_test.go` | The PR-tier test entry point. |
| `full_test.go` | The merge-tier test entry point, plus `provision()`/`runScript()` wrapping `scripts/provision.sh`/`teardown.sh`. |
| `regression_test.go` | Deterministic, infrastructure-free regression tests for the observable-baseline precondition and agent-stop verification, driven by a scripted `compute.Client` and a scripted guest-OS runner — they run in the GitHub-hosted `CI` verify job on every PR (`-run '^TestRegression_'`) and alongside both live e2e tiers, and need no VM. |
| `scripts/provision.sh`, `scripts/teardown.sh` | Shell scripts wrapping the `mws` CLI directly (not the Go SDK) to create/destroy the merge-tier's ephemeral VM+disk — see `docs/ci-cd.md`'s provisioning runbook for why a CLI wrapper instead of Terraform. |

To find where a given behavior is implemented: the scenario steps themselves are always in
`scenario.go`; what differs *between* the two tests is only in `smoke_test.go`/`full_test.go`
(assembling those steps, and where `Target` comes from).

### Scenario coverage — what's actually implemented today

`docs/design.md` §8.3 lists ten scenarios as the accepted end state. Today, **three** of them are
covered — scale-up on saturation, plus both write-path rows: each tier exercises its own real write
path (dependent on the smoke tier's persistent disk, independent on the full tier's ephemeral disk),
with the disk's class asserted (`AssertDiskClass`) rather than trusted from a naming convention. The
remaining seven are not yet separate test cases. Recorded here honestly rather than as a coverage
claim the code doesn't back up yet:

| §8.3 scenario | Status |
|---|---|
| Scale-up on saturation | Implemented, both tiers |
| Dependent write path | Implemented, smoke tier — the persistent VM's target disk is dependent (`docs/design.md` §4.7); its class is asserted by `Preflight` (`AssertDiskClass`), not trusted from the device-name convention |
| Independent write path | Implemented, full tier — the ephemeral disk is independent; its class is asserted at scenario start (`AssertDiskClass`), not trusted from the provisioning script |
| No scale-up when efficient | Not implemented |
| Cold start | Not implemented |
| Dry run | Not implemented |
| VM cap detection | Not implemented |
| Bandwidth-bound case | Not implemented |
| Scale-down after quiet | Not implemented — the feature itself doesn't exist yet (`allowScaleDown = false`, Phase 1 scope) |
| No flapping | Not implemented — depends on scale-down |

See `docs/TODO.md` for turning the remaining rows into real test cases as separate, explicit scenarios
rather than incidental side effects of which disk class a given persistent/ephemeral VM happens to use.

### Why preflight matters more than cleanup

The agent's 24-hour change budget (`internal/state/budget.go`) is deliberately durable across
restarts — a safety invariant, not a bug (see `docs/design.md` for why). Reusing one persistent VM
across every PR's smoke run would let one PR's scale-up events exhaust the next PR's budget or trigger
its cooldown, failing a run for reasons unrelated to the code under test.

The fix is not to weaken that invariant — it's to give each run its own state. The smoke test starts
the agent with `-state-dir` pointed at a fresh, run-specific directory
(`/tmp/mws-iops-e2e` on the shared VM, wiped by `Preflight` before each run), so every run's budget
file starts empty.

What still needs explicit handling is process/disk state left behind by a **crashed or cancelled**
prior run — this is why `Preflight` (not just `Cleanup`) is the actual correctness guarantee for the
shared persistent VM:

1. Stop any leftover agent process from a previous run — and verify it is actually gone: a kill command
   alone proves nothing (the agent handles SIGTERM and may take a moment to exit — or not exit on it at
   all), and a live orphan is the writer that re-raises the disk after a reset, timing its raises around
   any read-side confirmation interval (the agent's own 10s reconcile). `stopStaleAgent` re-kills and
   re-verifies absence with `pgrep` over bounded rounds, escalating SIGTERM rounds to SIGKILL — never a
   blind sleep. SIGKILL is safe here by design: the 24h change budget is a crash-safe intent log
   (docs/design.md §9.4) and the e2e state dir is per-run scratch, wiped by the next preflight.
   The absence check speaks a marker protocol, not raw output: `Target.Run` returns combined
   stdout+stderr, and with `UserKnownHostsFile=/dev/null` every ssh invocation prepends a host-key
   warning to it — so an exact-match comparison on that output can never succeed. Observed live: two
   smoke runs failed preflight with "agent still present" while the VM had no matching process at all.
   The check emits a unique final-line marker per pgrep outcome (present / absent / pgrep error, the
   last propagated as a non-zero remote exit too), parsed as the final non-empty output line;
   anything unrecognized fails closed.
2. Kill any leftover e2e-tagged `fio` process — matched narrowly, never a bare `pkill fio`, so it can't
   interfere with anything else using the VM.
3. Reset the target disk to baseline IOPS via the compute API if it isn't already there, following
   `docs/development.md`'s discovery/identity protocol (resolve by `metadata.id`, verify the guest
   device path — never assume the disk resolved last run is still the same incarnation).

Step 3's baseline is *observable*, not write-and-assumed, and the guarantee is deliberately split into
three layers, each scoped to what it can actually prove:

- **Process quiescence** (`stopStaleAgent`, step 1): the only writer the harness knows about — the e2e
  agent — is verified absent before the reset runs. This is what makes a periodic writer unable to
  quiet down for two reads and raise again: it is not alive to do it.
- **Eventual-consistency convergence** (`ResetDiskToBaseline` → `confirmBaseline`): neither a
  successful update response nor a single baseline reading proves the disk reads back at baseline —
  the platform's read path can serve a stale elevated value seconds after the write landed. Baseline
  is confirmed through **two consecutive fresh compute reads** separated by a short poll, bounded by a
  deadline: stale reads delay confirmation until they converge; a writer that never lets baseline
  settle exhausts the deadline. This layer is *scoped to convergence* — two reads seconds apart are
  not a quiescence proof against a periodic writer; that is step 1's job, and any raise that slips
  past this confirmation anyway is caught by the third layer.
- **Creditability preconditions** (`WaitForScaleUp`): the wait's **first observation must be the
  baseline itself**, and **any rise above baseline is rejected until the agent's `scale_up_window`
  (30s, the same constant that renders the agent's config) has elapsed since the agent started** — the
  agent cannot act before its evaluation window fills, so an earlier rise, even well after the wait
  began, cannot be organic. Only a rise at or after the window, above an observed baseline, is
  credited as this run's scale-up; everything else fails closed.

These layers exist because their absence was observed live, not theorized: a cancelled PR-tier run
left an orphaned agent on the shared VM that raised the disk above baseline; the next attempt's
preflight reset wrote baseline back, but the wait loop's *first* poll read the elevated value and
credited it as scale-up — an 11.9-second "pass" that never exercised the agent's evaluation window.
`e2e/regression_test.go` reproduces every failure mode deterministically on a scripted
`compute.Client` and a scripted guest-OS runner (stale-elevated reads after the reset write; a wait
loop starting above baseline; a rise before the scale-up window; an agent that keeps showing present
after kills; an absence check whose output is prefixed by the ssh host-key warning; a remote pgrep
failure or unrecognized check output), with injectable polling intervals and clocks so the tests run
in milliseconds. The
regression tests are infrastructure-free and run in three places: the GitHub-hosted `CI` verify job on
every PR (`-run '^TestRegression_'` — harness correctness is checked even when the live smoke tier
cannot run, e.g. fork PRs without secrets), and alongside the live scenarios in both e2e workflows.

`Cleanup` mirrors the same steps, attempts every one of them even after an earlier one fails, and
returns the aggregated error — which the tests fail on (`t.Errorf` from the defer), so a green check
never leaves the shared VM dirty. What "always runs" honestly means here is bounded: it runs via Go
`defer`, which covers ordinary test failures and panics, but not hard cancellation or runner loss —
neither e2e workflow has an `if: always()` infrastructure-cleanup step (the only `always()` step
deletes the SSH key file). A run killed that way can still leak processes or a raised disk, which is
exactly the orphan-sweeper gap recorded in `docs/TODO.md`; the next run's `Preflight` — not this
run's `Cleanup` — is what actually has to work for the shared VM to stay usable.

### Why the harness supervises the agent process directly

The agent's `--dry-run` and normal modes do not self-terminate — they run until `SIGTERM`. The e2e
harness starts the process, polls until the expected condition is observed (a real IOPS change via the
compute API, not the agent's own journal — the journal is written before actuation with no
write-outcome field, so a write failure can't be asserted from it alone), then sends `SIGTERM` and
waits for a clean shutdown, rather than assuming the process exits on its own. A graceful shutdown is
not guaranteed to arrive, though — the process may simply not exit on `SIGTERM` alone — which is why
the deferred cleanup's `stopStaleAgent` verifies absence and escalates to SIGKILL rather than trusting
the one signal.
