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
| `TestSmoke_Decisions_ScaleUpOnSaturation` | PR-tier ("smoke") | Reuses one persistent VM+disk |
| `TestFull_Lifecycle_ProvisionScaleUpTeardown` | Merge-tier ("full") | Provisions a fresh VM+disk, tears it down after |

### Test files group by subject, not by tier

Test files are named for what they cover (`decisions_test.go`, `cold_start_test.go`,
`independent_test.go`, `dependent_test.go`, `lifecycle_test.go`, `device_naming_test.go`,
`discovery_test.go`, `headroom_test.go`, and further subjects as more scenarios land —
guardrails), never for which tier runs them. Tier lives in the test *name* instead:
`Test<Tier>_<Group>_<Scenario>`, tier one of `Smoke`, `Full`, `Regression`. This is what lets one
scenario exist once and be picked up by whichever workflow's `-run` pattern matches its tier prefix,
instead of the same scenario forking into a smoke copy and a full copy the moment both tiers need it.

`TestRegression_Naming_ScenarioTestsMatchConvention` (`naming_test.go`) enforces this: it parses
every `e2e/*_test.go` file as source with `go/ast` (build tags don't affect parsing, so this needs no
live infrastructure) and fails on any function `go test` would run as a test whose name doesn't match
the convention. Without it, a name that drifts from the pattern is silently dropped from every
workflow's `-run` filter rather than failing loud — CI would just quietly stop running it.

**File map**:

| File | What it's for |
|---|---|
| `config.go` | `Target` (one VM+disk pair under test) and environment-variable loading (`SmokeTarget`, `Project`, `Zone`, `SkipOrFail`). |
| `ssh.go` | Guest-OS access primitives (`Run`, `RunBackground`, `RunBackgroundToLog`, `Copy`) over the system `ssh`/`scp` binaries — not a Go SSH library, since this is harness-only code never shipped in the production binary. `RunBackground` derives its output log path from the command itself (unique among concurrent background commands, not meant to be reconstructed by a caller); `RunBackgroundToLog` takes an explicit, stable path instead, for a caller that needs to read the output back later. |
| `reset.go` | Builds the compute-API client (`NewComputeClient`) and discovers/verifies/resets a disk (`AssertDiskClass`, `ResetDiskToBaseline`, `CurrentIOPS`) — shared by both tiers' preflight and cleanup. |
| `scenario.go` | The actual scenario steps: `Preflight`, `InstallAgent`/`InstallAgentWithDisks`, `StartAgent`, `FetchAgentLog`, `StopAgent`, `DriveLoad`/`DriveRateLimitedLoad`/`DriveBandwidthLoad`, `WaitForScaleUp`, `WaitForDecision`/`WaitForJournalRecord`, `AssertNoIOPSChange`, and `runAgentScenario`/`runAgentScenarioWithDisks` (build+install+start+stop, wrapping `run(agentStarted)` with journal-on-failure diagnostics). **This is where "what a test does" lives** — every test assembles these in the same order. `InstallAgent`/`runAgentScenario` are thin wrappers over the `*WithDisks` forms, defaulting to `SelectedDisksSpec(target.DeviceName)` at `defaultDiscoveryInterval` — today's exact original behavior, unchanged by `DisksSpec`'s addition. `runAgentScenarioWithDisks` is itself a thin `*testing.T`-facing wrapper over `runAgentScenarioWithDisksErr`, which returns an error instead of calling `t.Fatalf` — the primitive an `xfail`-wrapped scenario needs, since Go cannot catch a `*testing.T` failure raised from inside a test. The agent's stdout/stderr is captured at the stable path `StartAgent` passes to `RunBackgroundToLog` (`FetchAgentLog` reads it back), not at `RunBackground`'s auto-derived one. |
| `build.go` | Cross-compiles the agent binary for the target's guest OS (linux/amd64) regardless of what the runner itself is. |
| `decisions_test.go` | Scale-up/scale-down decision scenarios against the persistent VM's dependent disk: `TestSmoke_Decisions_ScaleUpOnSaturation` (also exercises the dependent write path as a side effect — see the coverage table below), `TestSmoke_Decisions_NoScaleUpWhenEfficient` (a light, sustained, sub-threshold load, gated on a liveness-proving journal record before crediting "no change" as meaningful — see below), `TestSmoke_Decisions_ScaleUpOnBandwidthSaturation` (a bandwidth-bound, ops-light load; asserts saturation was *detected*, not that a scale-up necessarily follows). |
| `cold_start_test.go` | `TestSmoke_Decisions_ScaleUpFromColdStart` — load is driven *before* the agent starts, so its first samples already show saturation with no established baseline; the one scenario where `DriveLoad` deliberately precedes `runAgentScenario` rather than living inside its callback. |
| `independent_test.go` | The independent write path on the PR tier: `TestSmoke_Independent_ScaleUpOnSaturation` (write path) and `TestSmoke_Independent_DryRunValidateOnly` (US-013's server-confirmed dry-run verdict), both against the persistent VM's second, independent disk (`SmokeIndependentTarget`). |
| `dependent_test.go` | `TestSmoke_Dependent_DryRunComputedOnly` — the dependent disk's dry-run counterpart to `independent_test.go`'s: no server-side validation exists for this write path (§4.7), so this asserts the decision is still reached and the disk is still never mutated. |
| `lifecycle_test.go` | Provisioning and teardown: `TestFull_Lifecycle_ProvisionScaleUpTeardown`, the merge-tier entry point, plus `provision()`/`runScript()` wrapping `scripts/provision.sh`/`teardown.sh`, the lazy `fullFixture` (`ensureFullFixture`/`fullTarget`/`fullNoDeviceNameTarget`) that provisions the merge tier's shared ephemeral VM and its three disks — one explicitly named, one left unnamed to default, one (churn) created but left unattached for `churn_test.go`'s live attach/detach scenarios — once per process (`sync.Once`) no matter how many full-tier tests ask for one, and the package's `TestMain`, which tears that fixture down exactly once after every test has run. |
| `device_naming_test.go` | `TestFull_DeviceNaming_NoDeviceNameDefaultsToAlias` — the merge tier's second disk, attached with no `deviceName` at all, on the same shared VM as `lifecycle_test.go`'s explicitly-named one; both forms of `docs/platform-reference.md`'s guest-device-mapping default are exercised side by side. Not a `naming_test.go` naming-convention concern — a live platform behavior. |
| `naming_test.go` | The naming-convention guard described above. |
| `churn_test.go` | Live attach/detach for the merge tier's churn disk: `setStorageDisks` resends the VM's whole `--storage-disks` list via the `mws` CLI (`compute.Client` deliberately exposes no attach/detach capability — production code never attaches or detaches a disk), since that list is the only way to change one disk's attachment without dropping the others (`docs/ci-cd.md`); `AttachChurnDisk`/`DetachChurnDisk` wrap it with the fixture's own known-good entries; `waitForDiskAttached` polls past the update's propagation lag. |
| `discovery_test.go` | The `disks.selected`/`disks.exclude` discovery-and-scope scenarios: `TestFull_Discovery_AttachedDiskManagedWithoutRestart` (auto-discovery picks up a disk attached after the agent has already started, within one `discovery_interval`, no restart) and `TestFull_Discovery_ExcludeUnresolvedNameIsFatalAtColdStart` (an unresolved `disks.exclude` name refuses to manage anything at cold start — `docs/design.md` §4.1). See "Discovery and scope coverage" below for what's implemented here versus proven only at the unit level so far. |
| `headroom_test.go` | `TestFull_Headroom_UnmanagedSiblingCountsTowardVMCap` (E5) — an unmanaged sibling doing real, sustained I/O must still count toward a managed disk's VM headroom (`docs/design.md` §4.6, D4/US-009). Runs unconditionally as an ordinary required scenario (see "VM headroom coverage" below); no longer `xfail`-wrapped. |
| `xfail.go` | `xfail(t, issueURL, scenario)` — converts a `func() error` scenario's outcome into a non-blocking `t.Skip` on error (a known, tracked gap) or a forced `t.Fatal` on nil (the fix shipped; remove this wrapper). The scenario it wraps must never call a `*testing.T` failure method itself — Go cannot catch one raised from inside a test — so it is built from `runAgentScenarioWithDisksErr` and the package's other error-returning primitives, never `runAgentScenarioWithDisks` or a bare `t.Fatalf`. Currently unused — available for the still-unimplemented §8.3 scenarios listed above. |
| `regression_test.go` | Deterministic, infrastructure-free regression tests for the observable-baseline precondition and agent-stop verification, driven by a scripted `compute.Client` and a scripted guest-OS runner — they run in the GitHub-hosted `CI` verify job on every PR (`-run '^TestRegression_'`) and alongside both live e2e tiers, and need no VM. Grouped by mechanism: `TestRegression_Preflight_*` (baseline reset, stale-agent stop) and `TestRegression_WaitForScaleUp_*`/`TestRegression_WaitForScaleDown_*` (convergence and timing, both directions) and `TestRegression_Flapping_*` (the alternation count behind `AssertBoundedFlapping`). |
| `sweep_script_test.go` | `TestRegression_Sweep_*`: drives `scripts/sweep.sh` against a stub `mws` on `PATH` — age and ownership filtering, dry-run, ambiguous names and ages fail closed, partial teardown and idempotent reruns, list failures. Infrastructure-free, so it runs in the `CI` verify job. |
| `scripts/provision.sh`, `scripts/teardown.sh` | Shell scripts wrapping the `mws` CLI directly (not the Go SDK) to create/destroy the merge-tier's ephemeral VM+disk — see `docs/ci-cd.md`'s provisioning runbook for why a CLI wrapper instead of Terraform. |

To find where a given behavior is implemented: the scenario steps themselves are always in
`scenario.go`; what differs *between* tests is only in each subject file (assembling those steps,
and where `Target` comes from) — never in which tier runs them.

### Why the merge tier provisions its shared VM lazily

`ensureFullFixture` provisions the merge tier's ephemeral VM and its disks once, behind `sync.Once`;
`fullTarget(t)` and `fullNoDeviceNameTarget(t)` each return a `Target` for one of the two disks
attached at creation to every full-tier test in the process, and `TestMain` tears the fixture down
once after `m.Run()` returns. The fixture's third disk (churn) is created but left unattached — only
`discovery_test.go`'s live-attach scenario uses it, via `fix.churnTarget()` and
`AttachChurnDisk`/`DetachChurnDisk` directly, so it has no `fullChurnTarget(t)`-style wrapper yet. Two
things sharing one fixture buys, both necessary once more than one full-tier scenario exists:

- A `-run` selecting only some full-tier tests never pays for provisioning it doesn't need — the
  fixture is created on first actual use, not eagerly.
- Multiple full-tier tests share one VM instead of one each, the same economy the smoke tier already
  gets for free from its persistent target.

Isolation between full-tier tests sharing that VM comes from `Preflight`, not from a fresh VM per
test — the identical guarantee the smoke tier's shared persistent target already relies on (see
below). Teardown itself stays unconditional on `E2E_RUN_ID` (set once per workflow run, not generated
per test), so it still recovers a partially provisioned run exactly as it did before this fixture
existed.

### Scenario coverage — what's actually implemented today

`docs/design.md` §8.3 lists ten scenarios as the accepted end state. Today, **nine** of them are
covered — each disk's class asserted (`AssertDiskClass`) rather than trusted from a naming convention.
The remaining one (VM cap detection) depends on a fix that doesn't exist yet. Recorded here honestly
rather than as a coverage claim the code doesn't back up yet:

| §8.3 scenario | Status |
|---|---|
| Scale-up on saturation | Implemented, both tiers |
| Dependent write path | Implemented, smoke tier — the persistent VM's dependent disk (`docs/design.md` §4.7); class asserted by `Preflight` (`AssertDiskClass`), not trusted from the device-name convention |
| Independent write path | Implemented, both tiers — the persistent VM's second, permanently attached independent disk on the smoke tier (`TestSmoke_Independent_ScaleUpOnSaturation`), the ephemeral disk on the full tier; class asserted at scenario start either way |
| Dry run | Implemented for both disk classes, smoke tier — `TestSmoke_Independent_DryRunValidateOnly` observes the platform's real `ValidateOnly` verdict; `TestSmoke_Dependent_DryRunComputedOnly` observes the same `scale_up` journal record on the class with no server-side validation to confirm against (§4.7). Both confirm zero mutation via repeated fresh reads |
| No scale-up when efficient | Implemented, smoke tier (`TestSmoke_Decisions_NoScaleUpWhenEfficient`) — a real, sustained, sub-threshold load. Asserting only "IOPS never changed" would pass vacuously for a dead or crashed agent, so this first requires a `no_action`/`no_signal` journal record proving a full evaluation window was reached before crediting the no-change observation |
| Cold start | Implemented, smoke tier (`TestSmoke_Decisions_ScaleUpFromColdStart`) — load is already present when the agent starts, so its first samples show saturation with no established baseline yet |
| VM cap detection | Not implemented — this row is §8.3's original scenario, loading *both managed* smoke-tier disks past the pool together; already correctly summed today without needing any fix. See "VM headroom coverage" below for the separate, narrower unmanaged-sibling gap (D4/US-009, E5) |
| Bandwidth-bound case | Implemented, smoke tier (`TestSmoke_Decisions_ScaleUpOnBandwidthSaturation`) — a large-block, high-depth load that saturates bandwidth while ops stay under the IOPS-utilization threshold. Asserts saturation was *detected* via the journal reasons only reachable from `decider.go`'s saturated branch (the IOPS-utilization reading is logged, not asserted on — a single newest-sample value, not a window average) — not that a scale-up necessarily follows, since `demand()`'s ops-denominated estimate may legitimately fail to clear the scale-up ratio against a bandwidth-bound workload. A `target_not_above_current` outcome here is a real, reportable gap in the demand estimator, not a test bug |
| Scale-down after quiet | Implemented, smoke tier (`TestSmoke_Decisions_ScaleDownAfterQuiet`) — elevates the disk directly (`ResetDiskToBaseline` against a non-default value, the same production write path `Preflight`/`Cleanup` already use), then starts the agent against an idle disk and asserts the limit drops, never below `min_iops` |
| No flapping | Implemented, smoke tier (`TestSmoke_Decisions_NoFlapping`) — drives two full up/down cycles (`proveUpDownCycle`, each requiring a real observed scale-up *and* scale-down before it counts, so a decider that never actuates can't pass vacuously) and asserts the journal shows no more `scale_up`/`scale_down` alternations than two clean cycles can account for (`AssertBoundedFlapping`) |

See `docs/TODO.md` for VM cap detection and the remaining Phase 3 gate (soak/chaos, calibration, and
the production flag flip itself).

### Discovery and scope coverage — what's actually implemented today

`docs/design.md` §4.1's cold-start-vs-runtime degradation model (an unresolved `disks.selected` or
`disks.exclude` name is fatal on the very first discovery of the process's life, degrades only that
name afterward) is fully covered at the unit level
(`internal/registry/registry_test.go`, `cmd/mws-iops-autoscaler/agent_test.go`). `discovery_test.go`
adds the two live scenarios that needed real infrastructure to prove: attaching a disk without a
restart, and the process actually refusing to start rather than merely returning an error a unit test
can catch:

| Scenario | Status |
|---|---|
| Auto-discovery picks up a live-attached disk within one `discovery_interval`, no restart | Implemented (`TestFull_Discovery_AttachedDiskManagedWithoutRestart`) |
| An unresolved `disks.exclude` name is fatal at cold start | Implemented (`TestFull_Discovery_ExcludeUnresolvedNameIsFatalAtColdStart`) |
| An unresolved `disks.exclude`/`disks.selected` name degrades without being fatal once at least one discovery has already succeeded | Proven at the unit level only (`TestReconcile_degradesPartiallyWhenASelectedDiskIsRemovedAtRuntime`) — shares `discover()`'s decision path with the cold-start case above, so no new product code path needs live proof yet |
| Auto-discovery drops a live-detached disk, other managed disks keep scaling | Not implemented |
| An excluded disk being attached or detached has no effect on the managed set | Not implemented |
| Moving a disk into or out of `disks.exclude`/`disks.selected` (config change, requires a restart — `docs/TODO.md`'s config hot-reload item) | Not implemented |
| `n+1` disks attached, `n` selected → exactly `n` managed | Not implemented |
| A `disks.selected` disk never attached at cold start → fatal, nothing managed | Proven at the unit level only (`TestDiscover_unmatchedSelectedDeviceAtColdStartIsFatal`) — same reasoning as the exclude row above: the exclude scenario already exercises this code path live, and `selected` shares it |
| Adding an already-attached disk to `disks.selected` (config change, requires a restart) | Not implemented |

The unimplemented rows all reuse `churn_test.go`'s attach/detach plumbing and `discovery_test.go`'s
`DisksSpec`-parameterized config; nothing further needs building in the harness itself to add them.

### VM headroom coverage

`docs/design.md` §4.6 defines VM headroom from "other disks", not "other **managed** disks" — the
VM's shared IOPS pool is consumed by every attached disk, whether or not this agent happens to manage
it. `TestFull_Headroom_UnmanagedSiblingCountsTowardVMCap` (`headroom_test.go`, E5 in the test-plan
shorthand, [issue #54][issue-54]) asserts exactly that: `disks.selected` names only the managed disk,
an unmanaged sibling (the churn disk) is driven with sustained load, and the managed disk's own
`vm_headroom_iops` journal field must fall meaningfully below the VM's full pool.

[Issue #54][issue-54]'s fix (`cmd/mws-iops-autoscaler/agent.go`'s `discover()`/`reconcile()`: every VM
disk outside the managed set is now sampled and analyzed too, feeding `siblingP95` without ever being
decided, actuated, or journaled itself) is covered directly at the unit level
(`TestDiscover_populatesUnmanagedFromDisksOutsideSelection`,
`TestReconcile_vmHeadroomCountsUnmanagedSiblingConsumption`) — proven deterministically there, the
same way the cold-start/runtime degradation split above is. The scenario ran under R4.5's `xfail`
pattern (`xfail.go`) until [issue #57][issue-57] removed that wrapper on the strength of #54's unit
coverage; the wrapper's own removal condition (a passing run forcing its own deletion) was therefore
never triggered live. This scenario now runs unconditionally, like every other required full-tier
scenario, and its first live confirmation has since happened: a `workflow_dispatch` full-tier run
passed `TestFull_Headroom_UnmanagedSiblingCountsTowardVMCap` in 71.74s
([run 34458884280](https://github.com/rodion1010/mws-iops-autoscaler/actions/runs/34458884280),
2026-09-10), alongside every other full-tier scenario including both of `discovery_test.go`'s — so
neither carries the "not yet run against live infrastructure" caveat anymore.

[issue-54]: https://github.com/rodion1010/mws-iops-autoscaler/issues/54
[issue-57]: https://github.com/rodion1010/mws-iops-autoscaler/issues/57

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
the smoke workflow has no `if: always()` infrastructure-cleanup step, and the full workflow's backstop
teardown only covers `go test`'s own `-timeout`. A run killed that way can still leak processes or a raised disk, which is
exactly the gap the orphan sweeper (`docs/ci-cd.md`) does not cover, since it only deletes full-tier VMs and disks; the next run's `Preflight` — not this
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
