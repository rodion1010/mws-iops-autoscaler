# CLAUDE.md

Repository instructions for coding agents. This file defines how to work in the repository; it is
not an architecture or platform reference.

## Read before changing code

- Product requirements and MVP scope: [docs/prd-mws-iops-autoscaler.md](docs/prd-mws-iops-autoscaler.md)
- Current architecture and safety invariants: [docs/design.md](docs/design.md)
- User-facing configuration contract: [docs/configuration.md](docs/configuration.md)
- Verified MWS API, SDK and monitoring behaviour: [docs/platform-reference.md](docs/platform-reference.md)
- Local setup and the reference test environment: [docs/development.md](docs/development.md)
- Known upstream limitations and temporary workarounds: [PLATFORM_DEPENDENCIES.md](PLATFORM_DEPENDENCIES.md)
- Deferred ideas: [docs/TODO.md](docs/TODO.md)

If documents contradict one another, surface the conflict instead of silently choosing an
interpretation. `TODO.md` is a parking lot, not authorized implementation scope.

## Change discipline

- Treat explicitly documented future work as normal WIP. It becomes a defect only when reachable
  behaviour violates the current MVP contract, a safety invariant, or an already implemented public
  contract.
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
- `docs/design.md`: the current architecture, flows, contracts and safety invariants.
- `docs/configuration.md`: the user-facing configuration schema, defaults and examples.
- `docs/platform-reference.md`: verified behaviour of external MWS APIs, SDKs and metrics.
- `PLATFORM_DEPENDENCIES.md`: upstream limitations, ownership, workarounds and removal conditions.
- `docs/development.md`: developer setup and test-environment conventions.
- `docs/TODO.md`: uncommitted ideas and deferred work.
- ADRs: historical rationale for exceptional architectural decisions.

Update every document whose own contract changed, but do not copy the same statement into the PRD,
design, ADRs and code comments. Link to the canonical document instead.

## Code comments

- Explain a non-obvious reason, invariant, external constraint or failure mode. Do not narrate what
  readable code already says.
- Keep comments local and durable. One to three lines is normally enough; longer rationale belongs in
  `docs/design.md` or `docs/platform-reference.md`.
- Do not put issue or PR chronology, review conversations, acceptance criteria, complete reproduction
  scenarios or amendment history in production comments.
- Put regression scenarios in descriptive test names and table cases. Use an issue for operational
  impact and reproduction steps.
- GoDoc on exported declarations describes the public contract, including meaningful error,
  concurrency and mutation semantics. Internal declarations do not need comments merely to repeat
  their names.
- A comment that no longer matches the implementation is a defect: update or remove it in the same
  change.
- Link to a stable document section when external evidence matters; do not link to line numbers.

## ADR policy

The default is to update `docs/design.md`, not to create an ADR. Create a new ADR only when all of
the following are true:

1. At least two credible alternatives were considered.
2. The decision crosses component boundaries or changes a public contract.
3. Reversing it later would be materially expensive.
4. The rationale will remain useful after the implementation and issue are closed.

Keep an ADR concise: Context, Decision, Alternatives and Consequences. It may link to the canonical
design section, but must not duplicate the current implementation walkthrough, test plan, issue
chronology, progress checklist or amendment log. `docs/design.md` remains the source of truth for
the current system. When a retained ADR is replaced, mark it superseded and link to the replacement.

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
