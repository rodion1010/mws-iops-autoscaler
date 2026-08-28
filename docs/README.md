# Documentation

| Document | Role |
|---|---|
| [`design.md`](design.md) | **Authoritative.** Architecture, flows, contracts, NFR, observability, testing, implementation plan |
| [`adr/`](adr/) | **Authoritative.** Individual decisions with alternatives and consequences |
| [`prd-mws-iops-autoscaler.md`](prd-mws-iops-autoscaler.md) | Requirements: user stories, functional requirements, defaults, success metrics |
| [`configuration.md`](configuration.md) | Policy model, presets, inheritance, examples, validation rules |
| [`TODO.md`](TODO.md) | Parking lot. **Nothing is picked up from here without an explicit decision.** |
| [`../CLAUDE.md`](../CLAUDE.md) | Measured platform behaviour — every empirical claim above is sourced here |

## Sync rule

`design.md` and the ADRs are the newer, more authoritative layer. **When a decision changes there,
the PRD and `configuration.md` are updated in the same change — always, without being asked.**

Documents that contradict each other are worse than a single document: a reader picking up the PRD
alone cannot tell it has been superseded, and will build the wrong thing. This has already happened
twice in this project — the utilization trigger and the "independent disks only" scope both survived
in the PRD after ADRs had overturned them.

When syncing, grep for the superseded statement and fix **every** occurrence: user stories,
functional requirements, defaults tables, success metrics, open questions.
