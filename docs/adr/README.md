# Architecture Decision Records

| ADR | Decision | Status |
|---|---|---|
| [001](ADR-001-metric-source-per-mode.md) | Metric source chosen by mode: `local` → guest, `fleet` → platform | Accepted |
| [002](ADR-002-saturation-not-utilization.md) | Scale on confirmed saturation, not on utilization | Accepted |
| [003](ADR-003-disk-write-paths.md) | Two write paths: disk spec for independent, VM spec for dependent | Accepted |

Requirements live in `docs/prd-mws-iops-autoscaler.md`. Measured platform behaviour lives in
`CLAUDE.md`. Architecture and testing strategy are in `docs/design.md`; configuration in `docs/configuration.md`.
