# CLAUDE.md

Repository instructions for coding agents. This file defines how to work in the repository; it is
not an architecture or platform reference.

## Repositories

Development happens in the private repository `rodion1010/mws-iops-autoscaler-private`: all branches,
pull requests, CI and the self-hosted runner live there. The public `rodion1010/mws-iops-autoscaler` is
a generated mirror of its `main` with cleaned history ([docs/ci-cd.md](docs/ci-cd.md), "Public mirror").

- Never push, branch or open a pull request in the public repository; never force-push it.
- Publish only after a change is merged to private `main`, from a checkout whose `HEAD` equals the
  remote `main` (compare SHAs after fetching):
  `SRC=https://github.com/rodion1010/mws-iops-autoscaler-private.git DST=https://github.com/rodion1010/mws-iops-autoscaler.git mirror/publish.sh`.
  Then clone the public repository and check its history before announcing the publish.
- A string that must not be public (internal host, ticket, project ID, personal email) goes into
  `mirror/replacements.txt`, and an author identity into `mirror/mailmap`, before the next publish.
  Both apply to the whole history.
- `mirror/` and `docs/platform-dependencies.md` never reach the mirror; public documents must not link
  to them.
- A public pull request is ported by hand to a private branch, verified there and published with the
  next mirror update.


- Product requirements and MVP scope: [docs/prd-mws-iops-autoscaler.md](docs/prd-mws-iops-autoscaler.md)
- Accepted architecture and safety invariants: [docs/design.md](docs/design.md)
- User-facing configuration contract: [docs/configuration.md](docs/configuration.md)
- Verified MWS API, SDK and monitoring behaviour: [docs/platform-reference.md](docs/platform-reference.md)
- Local setup and the reference test environment: [docs/development.md](docs/development.md)
- How the test tiers are implemented and organized: [docs/testing.md](docs/testing.md)
- The CI/CD pipeline that runs them: [docs/ci-cd.md](docs/ci-cd.md)
- Known upstream limitations and temporary workarounds: [docs/platform-dependencies.md](docs/platform-dependencies.md) (private repository only; not in the public mirror)
- Deferred ideas: [docs/TODO.md](docs/TODO.md)

If documents contradict one another, surface the conflict instead of silently choosing an
interpretation. `TODO.md` is a parking lot, not authorized implementation scope.

## Change discipline

- Treat explicitly documented future work as normal WIP. It becomes a defect only when reachable
  behaviour violates the current MVP contract, a safety invariant, or an already implemented public
  contract.
- Treat `docs/design.md` as the normative, currently accepted architecture, not a code-completion
  ledger. Track implementation deviations in issues; do not weaken an invariant or add gap status to
  the design merely to mirror incomplete code.
- Keep functional changes separate from comment-only or documentation-only cleanup.
- Do not widen the requested scope merely because a nearby TODO or design idea exists.
- Preserve fail-closed behaviour around disk identity, stale evidence, concurrency and change-budget
  accounting. Read the relevant section of `docs/design.md` before changing these paths.
- After moving or renaming a documented concept, search the whole repository and update every
  reference to its canonical location.

## Documentation ownership

Each fact has one canonical home:

- `docs/prd-mws-iops-autoscaler.md`: product requirements, supported scope, priorities and success
  criteria.
- `docs/design.md`: the accepted architecture, flows, contracts and safety invariants the implementation must satisfy.
- `docs/configuration.md`: the user-facing configuration schema, defaults and examples.
- `docs/platform-reference.md`: verified behaviour of external MWS APIs, SDKs and metrics.
- `docs/platform-dependencies.md`: upstream limitations, ownership, workarounds and removal conditions.
- `docs/development.md`: developer setup and test-environment conventions.
- `docs/testing.md`: how the unit/integration/e2e tiers are implemented — what each covers, how to run
  it, and how the test code is organized.
- `docs/ci-cd.md`: the CI/CD pipeline — verification tiers, runner and service-account infrastructure,
  IAM, and the provisioning runbook.
- `docs/TODO.md`: uncommitted ideas and deferred work.
- ADRs: temporary decision requests from an agent to the Product Lead; never accepted architecture.

Update every document whose own contract changed, but do not copy the same statement into the PRD,
design, ADRs and code comments. Link to the canonical document instead.

## Code comments

- Explain a non-obvious reason, invariant, external constraint or failure mode. Do not narrate what
  readable code already says.
- Keep comments local and durable. One to three lines is normally enough; longer rationale belongs in
  `docs/design.md` or `docs/platform-reference.md`.
- No history in comments, production or test: no issue, PR, user-story or milestone numbers, no
  review conversations, acceptance criteria, reproduction scenarios or amendment history ("used to",
  "previously", "after the fix").
- No references to documents (`docs/*.md`, `§N`) in comments. State the reason itself in one plain
  sentence; the full rationale lives in the documents, which point to the code, not the other way.
- Put regression scenarios in descriptive test names and table cases. Use an issue for operational
  impact and reproduction steps.
- GoDoc on exported declarations describes the public contract, including meaningful error,
  concurrency and mutation semantics. Internal declarations do not need comments merely to repeat
  their names.
- A comment that no longer matches the implementation is a defect: update or remove it in the same
  change.
- A comment-only change must not change code: check that each Go file's token stream without
  comments is identical before and after, and that shell and YAML diffs touch only `#` lines.

## User-visible strings

Error messages, log lines, flag help, Prometheus HELP texts and test failure messages follow the same
rules as comments: no issue, PR, user-story or milestone numbers, no `docs/*.md` or `§N` references, no
history. Say what happened and, where useful, what to do. For example, `resolve own VM identity
(US-002)` becomes `resolve own VM identity`, and `requires PlatformSource, which ships in M7` becomes
`"platform" is not implemented yet`.

## ADR policy

An ADR is a temporary communication artifact, not a permanent architecture record. Use one only when
an agent finds a material choice that the PRD, current design and verified platform facts do not
resolve, and needs the Product Lead to choose how to proceed.

Before creating an ADR:

1. Try to resolve the question from the canonical documents and current code.
2. If the choice is local, reversible and does not change a contract, make the reasonable engineering
   choice without creating an ADR.
3. Create an ADR only when at least two credible options remain and the choice materially affects
   product behaviour, a public contract, safety or multiple components.
4. Do not use an ADR merely to explain an implementation, preserve issue history or announce a
   decision the agent has already made.

Every ADR must have status `Proposed` and contain: the unresolved question, relevant context and
constraints, credible options with trade-offs, the agent's recommendation, and the exact decision
requested from the Product Lead. Keep it concise and do not start implementation that depends on the
choice until the decision is made.

After the Product Lead decides, update the appropriate canonical documents and delete the ADR in the
same change. The PR or issue discussion and Git history preserve the communication. Do not keep
`Accepted` ADRs in the active tree and do not use ADRs as a parallel history of the architecture.

## Verification

- Every bug fix includes a regression test that fails for the reported scenario before the fix and
  asserts the externally observable result.
- Run `gofmt` on changed Go files.
- Run `go test ./...`, `go vet ./...` and `go test -race ./...` for functional Go changes.
- For documentation-only changes, verify relative links and search for stale names or moved sections.

## Credentials and environment-specific data

Never commit access tokens, service-account keys, private keys, generated credential files, fixed SSH
endpoints or local secret paths. Use the standard MWS credential chain and keep key material outside
the repository. A cloud coding session must receive credentials through its secret store or
environment injection, never through a committed file or chat transcript.
