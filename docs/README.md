# Documentation

| Document | Canonical responsibility |
|---|---|
| [`prd-mws-iops-autoscaler.md`](prd-mws-iops-autoscaler.md) | Product requirements, MVP scope, priorities and success criteria |
| [`design.md`](design.md) | Current architecture, flows, contracts, safety invariants, observability and testing strategy |
| [`configuration.md`](configuration.md) | User-facing policy model, defaults, validation rules and examples |
| [`platform-reference.md`](platform-reference.md) | Verified behaviour of external MWS APIs, SDKs, metrics and billing |
| [`development.md`](development.md) | Credential handling, developer setup and the reference test environment |
| [`../PLATFORM_DEPENDENCIES.md`](../PLATFORM_DEPENDENCIES.md) | Upstream gaps, temporary workarounds and removal conditions |
| [`TODO.md`](TODO.md) | Parking lot; nothing is implemented from here without an explicit decision |
| [`adr/`](adr/) | Historical rationale for exceptional architectural decisions |
| [`../CLAUDE.md`](../CLAUDE.md) | Repository instructions for coding agents |

## Source-of-truth model

The PRD is primary for *what* the product must do and *why*. `design.md` is primary for how the
current system satisfies those requirements. Neither document is the source for measured external
platform behaviour: those facts live in `platform-reference.md`.

A requirement exists once it is in the PRD. A design document cannot silently introduce, widen or
drop a requirement. Conversely, an implementation detail does not belong in the PRD merely because
it is important to the code.

ADRs preserve rationale when the decision meets the criteria in `CLAUDE.md`; they are not the
canonical description of the current architecture. A reader should be able to understand the current
system from `design.md` without reconstructing a chain of ADR amendments.

## Update rule

Use one canonical home for each statement and link to it from other documents. Do not copy the same
rule into the PRD, design, ADRs, configuration guide and code comments.

A change updates only the documents whose own contracts changed:

1. Change the PRD when product scope, behaviour, priority or acceptance criteria change.
2. Change `design.md` when architecture, a flow, a contract or a safety invariant changes.
3. Change `configuration.md` when the user-facing schema, default or validation behaviour changes.
4. Change `platform-reference.md` when an external fact is verified or corrected.
5. Change `PLATFORM_DEPENDENCIES.md` when an upstream limitation, status or workaround changes.
6. Add an ADR only for an exceptional decision that satisfies the repository's ADR policy.

If a measured platform fact invalidates a requirement or design, update the fact's canonical record
first, then update every affected contract. After moving or replacing a statement, search the whole
repository for the old wording and repair all references.
