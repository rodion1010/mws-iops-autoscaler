# Documentation

| Document | Role |
|---|---|
| [`prd-mws-iops-autoscaler.md`](prd-mws-iops-autoscaler.md) | **Primary.** What we are building and why: user stories, functional requirements, defaults, success metrics |
| [`design.md`](design.md) | Derived from the PRD. How it is built: architecture, flows, contracts, NFR, observability, testing, implementation plan |
| [`adr/`](adr/) | Derived from the PRD. Individual decisions with alternatives and consequences |
| [`configuration.md`](configuration.md) | Derived from the PRD and design: policy model, presets, inheritance, examples, validation rules |
| [`TODO.md`](TODO.md) | Parking lot. **Nothing is picked up from here without an explicit decision.** |
| [`../CLAUDE.md`](../CLAUDE.md) | Measured platform behaviour — every empirical claim above is sourced here |

## Document hierarchy

**The PRD is primary.** It is the source of requirements, scope and priorities — it answers *what*
and *why*. `design.md` and the ADRs are derived: they answer *how*. `configuration.md` is derived
from both.

A requirement exists once it is in the PRD. A design document cannot introduce, widen or silently
drop a requirement on its own.

## Sync rule

**Any change is written into the PRD, `design.md` and the ADRs in the same change — always, without
being asked.** `configuration.md` follows whenever the change touches the user-facing surface.

This holds regardless of where the change originated. In particular, when a change is discovered
from below — a measured platform fact invalidates a requirement, as in ADR-002 where the broken
`*_quota_utilization` metrics killed the utilization trigger — the direction of the edit changes,
not which document is primary:

1. Amend the **PRD** first: the requirement itself is what changed.
2. Record the reasoning and the alternatives in an **ADR**.
3. Update **`design.md`** to match, and `configuration.md` if the surface moved.

Documents that contradict each other are worse than a single document: a reader picking up the PRD
alone cannot tell it has been superseded, and will build the wrong thing. This has already happened
twice in this project — the utilization trigger and the "independent disks only" scope both survived
in the PRD after ADRs had overturned them. Both are the failure this rule exists to prevent.

When syncing, grep for the superseded statement and fix **every** occurrence: user stories,
functional requirements, defaults tables, success metrics, examples, open questions.
