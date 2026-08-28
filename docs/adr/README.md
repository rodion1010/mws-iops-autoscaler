# Architecture Decision Records

| ADR | Decision | Status |
|---|---|---|
| [001](ADR-001-external-agent-over-public-apis.md) | External agent over public APIs, not a platform feature | Accepted |
| [002](ADR-002-metric-source-and-utilization.md) | Platform metrics primary; guest collection opt-in in `local` mode | Accepted |
| [003](ADR-003-reactive-decider-and-disk-scope.md) | Reactive sliding-window decider; independent disks only | Accepted |

Requirements live in `tasks/prd-mws-iops-autoscaler.md`. Measured platform behaviour lives in
`CLAUDE.md` — every empirical claim in these ADRs is sourced there.
