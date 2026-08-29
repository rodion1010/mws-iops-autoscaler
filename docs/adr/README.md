# Architecture Decision Records

| ADR | Decision | Status |
|---|---|---|
| [001](ADR-001-metric-source-per-mode.md) | Metric source chosen by mode: `local` → guest, `fleet` → platform | Accepted |
| [002](ADR-002-saturation-not-utilization.md) | Scale on confirmed saturation, not on utilization | Accepted |
| [003](ADR-003-disk-write-paths.md) | Two write paths: disk spec for independent, VM spec for dependent | Accepted |
| [004](ADR-004-disk-identifier-in-config.md) | Disks are addressed by `device_name`, never by kernel device path | Accepted |
| [005](ADR-005-local-before-fleet-sequencing.md) | Local mode ships complete before fleet mode; platform metrics validated via shadow mode | Accepted |
| [006](ADR-006-single-default-defer-presets.md) | Phase 1 ships one built-in default parameter set; named presets deferred | Accepted |
| [007](ADR-007-dependent-disk-write-has-no-dry-run.md) | `UpdateVirtualMachine` has no `validateOnly` — dependent-disk dry-run cannot be server-confirmed | Accepted |

ADRs are **derived from the PRD**, which is the primary document: it states the requirement, an ADR
records how the requirement is met and why that way. An ADR never introduces or drops a requirement
on its own — when a decision here changes one, the PRD is amended in the same change. See the sync
rule in `docs/README.md`.

Requirements live in `docs/prd-mws-iops-autoscaler.md`. Measured platform behaviour lives in
`CLAUDE.md`. Architecture and testing strategy are in `docs/design.md`; configuration in `docs/configuration.md`.
