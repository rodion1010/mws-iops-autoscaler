# Integration tests on GitHub runners and CI — design

Status: proposed for review
Branch: `integration-tests`

## 1. Problem

`docs/design.md` §8.2 defines an accepted integration-test layer — an in-process fake MWS API
replaying recorded responses — that has never been implemented. Nothing in the repository currently
exercises the SDK-to-platform boundary: `httptest` appears in three test files and only ever as
`httptest.NewRecorder()` for the `/metrics` handler. `internal/compute/sdk_client_test.go` asserts
the shape of a request body but never sends it. The entire path of real HTTP status codes, real
platform error bodies and generated response decoding is untested.

The repository also has no CI. It is private, so GitHub Actions bills against the Free plan's
2000 minutes per month.

## 2. Goals

- Implement every currently reachable scenario of `docs/design.md` §8.2 against an in-process fake
  MWS API — five of six; §6.2 records why the sixth is unreachable until M7.
- Prove on every pull request that the agent binary starts, wires up and shuts down on a runner.
- Add one GitHub Actions workflow running the verification commands `CLAUDE.md` already requires.

## 3. Non-goals

- No load generation, no `fio`, no loop devices, no `sudo`. Load-driven scenarios belong to §8.3
  (live test environment) and stay there; this design does not move them.
- No test against the real MWS platform, no credentials in CI.
- No production code change. In particular `metrics.GuestSource` keeps its unexported
  `procDiskstatsPath`/`resolveSymlink` fields; nothing is exported to make tests reachable.
- Fleet mode and the platform metrics source are unimplemented (Phase 4 and M7) and out of scope.

## 4. Verified platform and SDK facts

These are facts about an external SDK, so their canonical home is `docs/platform-reference.md`
(`CLAUDE.md` §Documentation ownership): see its `## Go SDK` section, subsections "Endpoint discovery
and transport [V]" and "Retry policy and error decoding [V]" — the environment seams, the discovery
call, that a plain `http://` base endpoint is accepted (with the `Content-Type` requirement), the
idempotency header name, the default retry policy and status codes, and the error-body `code`
requirement.

One fact stays here because it is about this repository's CI, not the platform: **CI can build the
project.** `go.mws.cloud/go-sdk@v0.19.0` and `util-toolset@v0.15.1` are served by `proxy.golang.org`
and verify against `go.sum`; confirmed by downloading both into an empty `GOMODCACHE`. The internal
host `git.example.internal` resolves to `198.51.100.10` and is unreachable publicly, but nothing needs it.

Two consequences shape the scenarios below. Transport errors are retried, so §8.2's injected-abort
scenario is exercisable. **409 is also retried**, so an etag-conflict test must assert the observable
outcome — no mutation — rather than a request count.

## 5. Approach

Approach C of three considered.

The §8.2 suite runs **in-process**, exactly as §8.2 words it: the test constructs the real `Agent`
with the real `compute.NewSDKClient`, pointed by environment at an `httptest` fake, and injects a
synthetic `metrics.MetricSource`. `metrics.NewSampler` already accepts that interface, so no
production seam is needed. Faking the metric source is deliberate: §8.2 is about the MWS API
boundary, and guest metrics are §8.3's subject.

A separate **black-box smoke test** builds and executes the real binary against the same fake. It
asserts only startup, HTTP surface and clean shutdown, so it needs no saturation and therefore no
control over `/proc/diskstats`.

Approach A — driving everything through the binary — was rejected because `metrics.NewGuestSource`
hardcodes `/proc/diskstats` and `/dev/disk/by-id/mws-<name>`. Without a production seam a disk fails
`guestPathVerifier`, leaves the managed set, and no §8.2 scenario ever reaches a write. Adding that
seam would be a production change made solely for tests, and would exceed §8.2's accepted contract.
Approach B — in-process only — was rejected because it never proves the binary runs on a runner.

## 6. Components

```
internal/platformfake/          fake MWS API and IMDS, exported for tests
cmd/mws-iops-autoscaler/integration_test.go    §8.2 scenarios and smoke, build tag `integration`
.github/workflows/ci.yml        the workflow
```

### 6.1 `internal/platformfake`

An `httptest.Server` pair — one for the Compute API and discovery, one for IMDS — behind a small
struct.

Responsibilities:

- Serve `GET /endpoint`, the five Compute paths and `GET /computeMetadata/v1/instance/name`.
- Hold seeded state: one VM with one dependent and one independent disk, plus a VM type.
- Marshal success responses from hand-written fixture types mirroring the subset of each
  `go.mws.cloud/go-sdk/service/compute/model` response that its translator in
  `internal/compute/sdk_client.go` actually reads, pinned by untagged tests (`fake_test.go`, no
  build tag, so they run under plain `go test ./...`) that round-trip these fixtures through the
  real SDK client rather than a hand-decoded assertion. Marshalling from the SDK's own model types
  was the original plan (§8 risk 2 as first written); it was rejected once implementation showed the
  generated *encoders* and *decoders* are not provably symmetric for every optional/oneOf field, so
  using the encoder would silently assume the exact thing this fake exists to verify. The round-trip
  approach caught three non-obvious wire shapes a model-based fake would have hidden: resource
  references (`vmType`, `disk.ref`) decode as bare strings, not the `{"disk": "..."}` object shape a
  first reading of the OpenAPI schema suggests; `vcpuCount` must be a quoted numeric string, not a
  bare number; and disk `size` must use an SI suffix (`"10GB"`), not IEC (`"10Gi"`), or it fails to
  decode. An SDK version bump must re-run these round-trip tests — a new generated decoder could
  silently accept or reject a fixture shape that used to be right.
- Serve error bodies verbatim from `docs/platform-reference.md` §"Server-side validation, measured"
  and §"Dependent vs independent disks", which are marked `[V]` (verified live).
- Record every **write** request — method, path, query, idempotency key, response status — for
  assertions. Read paths (`GetVirtualMachine`, `GetDisk`, `GetVmType`, discovery, IMDS) are not
  recorded. Consequence: no test can assert "no unexpected request reached the fake" — only "no
  unexpected *write* reached it."
- Expose injection hooks: fail the next N calls to a path with a given status and body, or abort the
  connection mid-response.
- Honour idempotency keys: a repeated key applies the change once and replays the first response.

It depends on nothing in the agent, so it can be understood and tested on its own.

### 6.2 Scenario coverage

`docs/design.md` §8.2 lists six scenarios. Five are implementable now; one is not, and this design
does not pretend otherwise.

| §8.2 scenario | Status | Test shape |
|---|---|---|
| dependent write → 403 → agent routes to VM path | implementable, reframed | Routing is decided by `DiskClass` at discovery (`internal/actuator/actuator.go`), not by a 403 fallback. The fake serves the recorded 403 on the disk-update path as a **trap**: the assertion is that a dependent disk's write never touches that path and lands on the VM path instead. |
| `iops` out of bounds → 400 with the platform's message | implementable, assertion corrected | Fake returns `400 Requested iops can't be greater than maximum iops value: 10000`. Assert: seeded state unmutated, and the budget reservation resolved from live state. **Not** "journals the failure" — `agent.go:284` writes the journal record *before* actuation and `journal.Record` has no write-outcome field, so a failure record does not exist. Asserting one would require a journal contract change, which §3 rules out. |
| etag conflict → abort, no write | implementable, assertion corrected | Fake returns 409 on update. Assert only the half that implementation and design agree on: **no mutation of seeded state**. See §8.5 — "abort" does not hold today and is an open question, not something this test should bake in either direction. |
| idempotency key replay → single effective change | implementable | Force a retry, assert the fake recorded one effective mutation and the same key twice. |
| 503 from the metrics endpoint with an IAM token → "wrong credential type" | **deferred** | Requires the platform metrics source, which ships in M7. `cmd/mws-iops-autoscaler/main.go:85` rejects `runtime.metrics.source: "platform"` outright today. There is no reachable code path to test. Recorded here, not silently dropped. |
| injected TLS abort mid-write → retry, exactly one applied change | implementable, scope narrowed | Fake aborts the connection on the first update; the SDK retries transport errors; assert exactly one effective mutation via the idempotency key. Over plain HTTP this proves the transport-error retry classification (`IsTransportError`), which is the same branch a TLS abort takes — but it is not literally `SSL_ERROR_SYSCALL`. The TLS-specific variant stays in §8.3 against the live environment. |

### 6.3 Smoke test

Builds the binary into `t.TempDir()`, runs it with `-config`, `-state-dir`, `-journal` and `-listen`
pointed at temporary paths and a port the test reserves, with `MWS_BASE_ENDPOINT`, `MWS_TOKEN`,
`MWS_PROJECT` and `MWS_COMPUTE_METADATA_HOST` pointing at the fake.

Asserts: the process starts, initial discovery succeeds against the fake, `/healthz` returns 200,
`/metrics` serves the registry, and `SIGTERM` produces a clean exit.

`/readyz` gets a precise assertion rather than "responds". `Readiness.ReadyzHandler` returns 200 only
when all four conditions hold and 503 with a JSON breakdown otherwise (`internal/health/health.go`).
On a runner every disk fails guest-path verification, so `sourceReachable` is false and the correct
expectation is **503 with `config_valid: true`, `discovery_succeeded: true`, `source_reachable:
false`**. That is a stronger assertion than a 200 would be: it proves discovery genuinely reached the
fake and that readiness reports the specific failing condition.

It does not assert on disks. On a runner `/dev/disk/by-id/mws-*` does not exist, so every disk fails
`guestPathVerifier` and the managed set is empty — non-fatal, per
`TestDiscover_allDisksFailingVerificationLeavesEmptyManagedSet`. This also makes the smoke test
portable to macOS, where `/proc/diskstats` is simply absent.

### 6.4 Assertion surfaces

- The decision journal (JSONL, `docs/design.md` §7.2) written to a temporary path.
- The fake's request log.
- The fake's seeded state, for proving that nothing was mutated.
- `/metrics`, `/healthz`, `/readyz` for the smoke test.

## 7. CI workflow

One file, `.github/workflows/ci.yml`, one job on `ubuntu-latest`.

- Triggers: `push` to `main` and `pull_request`.
- `concurrency` group per ref with `cancel-in-progress`, so superseded runs stop burning minutes.
- `actions/setup-go` with `go-version-file: go.mod` and module caching enabled.
- Steps, matching `CLAUDE.md` §Verification: `gofmt -l` (fails on any output), `go vet ./...`,
  `go test ./...`, `go test -race ./...`, then `go test -race -tags integration ./...` — the
  integration step also runs under `-race`, not plain `go test -tags integration ./...`.

Integration tests sit behind the `integration` build tag so `go test ./...` stays fast for local use.

Budget: a run is roughly four minutes with a warm module cache, against 2000 free minutes per month
on the Free plan for a private repository. Linux runners bill at a 1x multiplier; no Windows or macOS
job is proposed.

## 8. Risks and open questions

1. **Retry backoff and test duration.** 409 and transport errors are retried with jitter up to 10 s.
   If the etag and TLS-abort tests become slow, the options are a bounded `context` or building the
   test SDK with `mws.WithRetryer`. The latter diverges from production behaviour, so it is a
   deliberate trade-off to decide during implementation, not silently.
2. **Fake fidelity.** Success bodies are hand-written fixtures, not marshalled from SDK model types
   (see §6.1 — the encoder/decoder symmetry that would have made "built from SDK types" true was not
   available for every optional/oneOf field). They cannot drift structurally *undetected*, because
   they are pinned by round-trip tests through the real SDK decoders, and an SDK bump must re-run
   those tests. But a fixture that is semantically wrong in a way the decoder still accepts would
   still let tests pass while reality differs. Error bodies are pinned to the `[V]`-marked recordings
   in `docs/platform-reference.md`.
3. **Port selection in the smoke test.** Reserving a port then handing it to a child process is
   inherently racy. Acceptable for a single-job CI; the alternative is parsing the chosen port from
   the agent's startup log, which would require a production logging change.
4. **Fake drift over time.** The fake encodes today's platform behaviour. When the real API changes,
   green integration tests will not notice. This layer complements §8.3's live E2E; it does not
   replace it.

### 8.5 Open question — a server-side etag conflict does not abort the cycle

`docs/design.md` §8.2 states the expected behaviour as "etag conflict → abort, no write". The "no
write" half holds. The "abort" half does not.

`internal/compute/sdk_client.go:191` and `:234` translate an etag conflict into
`fmt.Errorf("etag conflict — …: %w", err)`, wrapping the SDK error rather than
`compute.ErrConcurrentModification`. `cmd/mws-iops-autoscaler/agent.go:353` matches only errors
wrapping `ErrConcurrentModification` or `ErrIncarnationMismatch`; anything else falls through to the
"unclassified error may be post-write" branch at `:378`, which resolves the reservation from fresh
state and lets the cycle continue. No rediscovery is forced.

`ErrConcurrentModification` is documented as a *pre-write* refusal, and a server 409 is a rejected
write, so the current classification is defensible rather than plainly wrong. But it does mean the
accepted design and the implementation disagree, which `CLAUDE.md` requires surfacing rather than
resolving unilaterally. Two credible resolutions:

- classify a server-side etag conflict as `ErrConcurrentModification`, since a 409 provably applied
  no mutation — a small production change, outside this branch's scope; or
- amend `docs/design.md` §8.2's wording to match the implemented, defensible behaviour.

Until that is decided, the integration test asserts only what both agree on: no mutation. This
belongs in an issue, not in an edit made by this branch.

## 9. Documentation impact

This design implements `docs/design.md` §8.2 as accepted and weakens no invariant. Two changes do
follow from it:

- **`docs/platform-reference.md`** gains the SDK facts verified in §4 — the environment seams, the
  discovery call, that a plain `http://` base endpoint is accepted, and the default retry policy
  including which status codes are retried. That file is their canonical home under `CLAUDE.md`
  §Documentation ownership, and §4 of this spec should shrink to a link once they land there.
- **`docs/development.md`** gains a line on running `go test -tags integration ./...`.

`docs/design.md` is not edited. Two findings become issues instead, per `CLAUDE.md`'s rule that
implementation deviations are tracked in issues rather than mirrored into the design:

1. §8.2's metrics-credential scenario is unreachable until M7 (§6.2).
2. A server-side etag conflict does not abort the reconcile cycle (§8.5).

## 10. Verification

`go test -tags integration ./...` locally and in CI, plus the four commands of `CLAUDE.md`
§Verification.
