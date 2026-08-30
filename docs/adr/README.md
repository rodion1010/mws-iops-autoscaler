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
| [008](ADR-008-bandwidth-bound-saturation-gate.md) | Saturation gate opens on IOPS or bandwidth utilization, not IOPS alone | Accepted |
| [009](ADR-009-minimal-dependent-disk-write.md) | Dependent-disk writes send a minimal partial body, never a full-spec resend | Accepted |
| [010](ADR-010-single-canonical-collection-interval.md) | One canonical sampling cadence (`runtime.metrics.collection_interval`); window readiness is timestamp-based | Accepted |
| [011](ADR-011-latency-statistic-capability-explicit.md) | Latency statistics carry their own derivation (windowed average vs. platform histogram), never forced uniform across sources | Accepted |
| [012](ADR-012-baseline-establishment-not-gated-by-saturation.md) | Baseline establishment is never blocked by saturation; only its updates are frozen | Accepted |
| [013](ADR-013-budget-persisted-cooldowns-stay-in-memory.md) | The 24h change budget must be persisted; cooldowns may safely stay in-memory | Accepted |
| [014](ADR-014-fail-closed-on-stale-evidence.md) | Fail closed on stale metrics and stale discovery snapshots — freshness/gap checks at the Analyzer, Decider, and Agent layers | Accepted |
| [015](ADR-015-conditional-direction-preserving-writes.md) | Both write paths re-read live state and abort on conflict before writing — never an unconditional absolute-value write | Accepted |
| [016](ADR-016-bandwidth-aware-scale-down-sizing.md) | Scale-down sizing uses the aligned per-sample max of IOPS and bandwidth demand, not `IOPSP95Down` alone | Accepted |
| [017](ADR-017-queue-unit-guard-before-iops-demand-math.md) | Queue-depth unit is checked, not assumed, before it enters IOPS demand math | Accepted |
| [018](ADR-018-disk-identity-key-for-long-term-state.md) | Samples, cooldown, and the change budget key on DiskName + IncarnationID, not DiskName alone | Accepted |
| [019](ADR-019-incarnation-safety-write-path-and-remaining-state.md) | Incarnation safety extended to the write precondition, guest counters, and the latency baseline | Accepted |


These records preserve the rationale that existed when each decision was made. They are not the
canonical description of the current system: that role belongs to `docs/design.md`. A reader must
not need to replay the ADR sequence to reconstruct current behaviour.

Create a new ADR only when the decision satisfies the criteria in `CLAUDE.md`. Product requirements
remain in `docs/prd-mws-iops-autoscaler.md`; verified platform behaviour is in
`docs/platform-reference.md`; configuration is in `docs/configuration.md`. When an ADR is
superseded, retain it only when its historical rationale remains useful and mark its status and
replacement explicitly.
