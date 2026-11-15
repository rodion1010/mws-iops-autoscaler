# Documentation

| Document | Canonical responsibility |
|---|---|
| [`prd-mws-iops-autoscaler.md`](prd-mws-iops-autoscaler.md) | Product requirements, MVP scope, priorities and success criteria |
| [`design.md`](design.md) | Accepted architecture, flows, contracts, safety invariants, observability and testing strategy |
| [`configuration.md`](configuration.md) | User-facing policy model, defaults, validation rules and examples |
| [`platform-reference.md`](platform-reference.md) | Verified behaviour of external MWS APIs, SDKs, metrics and billing |
| [`development.md`](development.md) | Credential handling, developer setup and the reference test environment |
| [`testing.md`](testing.md) | How the unit/integration/e2e test tiers are implemented: what each covers, how to run it, how the test code is organized |
| [`ci-cd.md`](ci-cd.md) | The CI/CD pipeline: verification tiers, service accounts and IAM, the provisioning runbook |
| [`platform-dependencies.md`](platform-dependencies.md) | Upstream gaps, temporary workarounds and removal conditions (private repository only; not in the public mirror) |
| [`TODO.md`](TODO.md) | Parking lot; nothing is implemented from here without an explicit decision |
| [`adr/`](adr/) | Temporary decision requests awaiting Product Lead input; normally empty |
| [`../CLAUDE.md`](../CLAUDE.md) | Repository instructions for coding agents |

## Source-of-truth model

The PRD is primary for *what* the product must do and *why*. `design.md` is the normative source for
the currently accepted architecture: how the system is intended to satisfy those requirements.
Neither document is the source for measured external platform behaviour: those facts live in
`platform-reference.md`.

The design is living: update it when an architectural decision changes. Implementation may
temporarily lag behind it; track that deviation in an issue rather than weakening the accepted
contract or turning the design into a completion-status document.

A requirement exists once it is in the PRD. A design document cannot silently introduce, widen or
drop a requirement. Conversely, an implementation detail does not belong in the PRD merely because
it is important to the code.

`design.md` contains every accepted architectural decision. ADRs are temporary proposals used only
when an agent needs the Product Lead to resolve a material ambiguity; after the decision, the ADR is
deleted and the result is written into its canonical document.

## Update rule

Use one canonical home for each statement and link to it from other documents. Do not copy the same
rule into the PRD, design, ADRs, configuration guide and code comments.

A change updates only the documents whose own contracts changed:

1. Change the PRD when product scope, behaviour, priority or acceptance criteria change.
2. Change `design.md` when architecture, a flow, a contract or a safety invariant changes.
3. Change `configuration.md` when the user-facing schema, default or validation behaviour changes.
4. Change `platform-reference.md` when an external fact is verified or corrected.
5. Change `platform-dependencies.md` when an upstream limitation, status or workaround changes.
6. Add a temporary ADR only when an unresolved material choice requires Product Lead input.

If a measured platform fact invalidates a requirement or design, update the fact's canonical record
first, then update every affected contract. After moving or replacing a statement, search the whole
repository for the old wording and repair all references.
