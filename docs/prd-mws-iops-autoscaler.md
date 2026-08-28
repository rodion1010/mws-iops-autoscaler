# PRD: MWS IOPS Autoscaler

Status: draft · Last updated: 2026-08-28
Platform facts referenced here live in `CLAUDE.md` at the repo root.

---

## 1. Introduction / Overview

On MWS Cloud Platform, a network disk (`NBS-PL2`) has an IOPS limit chosen by the user between
1000 and 10000. That limit is **billed as provisioned, not as consumed** — you pay for the number you
picked, whether or not the workload uses it.

This creates a permanent dilemma. Pick a low limit and the workload throttles during spikes. Pick a
high limit and you pay for headroom that sits idle most of the time. Today the only resolution is a
human watching graphs and editing the limit by hand.

The IOPS Autoscaler is an agent that closes that loop automatically: it observes disk load, and
raises or lowers the disk's IOPS limit within bounds the user sets. It runs in two modes — on the VM
whose disks it manages, or centrally against a fleet of VMs.

It is an **external agent built on public MWS APIs**. It does not modify NBS, the hypervisor, or any
platform-internal component.

---

## 2. Goals

- Keep disks out of throttling during load spikes without permanent over-provisioning.
- Reduce spend on idle IOPS headroom by scaling back down when load subsides.
- Let the user bound the behaviour: minimum, maximum, aggressiveness, and which disks are in scope.
- Never make things worse: no flapping, no unbounded spend, no scaling that cannot help.
- Detect and report the case where IOPS is the wrong lever (VM vCPU ceiling) instead of spending
  money on it.

---

## 3. User Stories

### US-001: Configuration file
**Description:** As an operator, I want a single declarative config file so that behaviour is
reproducible and reviewable.

**Acceptance Criteria:**
- [ ] YAML config with schema validation; unknown keys are an error, not silently ignored
- [ ] Every tunable in FR-13 is settable; omitted values fall back to documented defaults
- [ ] `--validate-config` exits non-zero with a human-readable message on invalid input
- [ ] Secrets (IAM token, API key) are read from env or file path, never inline in the config
- [ ] Typecheck/lint passes

### US-002: Disk discovery — on-VM mode
**Description:** As an operator running the agent on a VM, I want it to find that VM's disks by
itself so that I do not have to enumerate them.

**Acceptance Criteria:**
- [ ] Agent resolves its own VM identity from the metadata service
- [ ] Lists that VM's attached network disks via the Compute API
- [ ] Reads `deviceName` from `StorageDiskStatusResponse` and resolves the guest path as
      `/dev/disk/by-id/mws-{deviceName}` (falling back to `mws-{name}` when `deviceName` is absent)
- [ ] Verifies the resolved path exists before managing the disk
- [ ] Disks whose path does not resolve are skipped with a warning, not a crash
- [ ] Typecheck/lint passes

### US-003: Disk discovery — fleet mode
**Description:** As an operator, I want to name the VMs to manage in config so that one agent covers
a fleet.

**Acceptance Criteria:**
- [ ] Config accepts a list of VM identifiers
- [ ] For each VM, lists attached network disks via the Compute API
- [ ] A VM that does not exist or is unreachable is logged and skipped; other VMs still process
- [ ] Typecheck/lint passes

### US-004a: Classify disks as dependent or independent
**Description:** As the agent, I must know which disks I can actually change, because a disk owned by
its VM cannot be modified through the disk API at all.

**Acceptance Criteria:**
- [ ] For every discovered disk, reads `metadata.usages` and classifies it: `own` present →
      **dependent** (VM-owned); `attach` only → **independent**
- [ ] Independent disks are managed through `UpdateDisk` as normal
- [ ] Dependent disks are, for MVP, reported as unmanageable with the reason and excluded from
      scaling — never silently skipped
- [ ] The classification appears in logs, the dry-run report, and the exported metrics
- [ ] A disk that changes class between discovery cycles is reclassified, not cached forever
- [ ] Typecheck/lint passes

### US-004: Disk selection strategies
**Description:** As an operator, I want three ways to say which disks are in scope so that I can
opt in broadly or narrowly.

**Acceptance Criteria:**
- [ ] `all` — every discovered disk, re-scanned continuously so new disks are picked up
- [ ] `all_except` — every discovered disk minus an explicit exclusion list
- [ ] `explicit` — only the named disks
- [ ] Strategy is settable globally and overridable per VM
- [ ] Continuous rediscovery interval is configurable; new disks enter management without restart
- [ ] Typecheck/lint passes

### US-005: Metric source — platform monitoring API
**Description:** As the decider, I need disk metrics from the MWS monitoring API so that fleet mode
works without touching guests.

**Acceptance Criteria:**
- [ ] Fetches from `monitoring.mwsapis.ru` using the API key and `monitoring.metricExporter` role
- [ ] Parses Prometheus exposition format
- [ ] Extracts per-disk: read/write op counts (`_ops` and `_ops_peak`), read/write throughput,
      read/write latency histograms, and queue depth
- [ ] Missing or stale metrics mark the disk as "no data" — the decider must not act on it
- [ ] Typecheck/lint passes

### US-005a: Adopt platform metrics as they improve
**Description:** As the agent, I want to use the platform's own utilization and queue-depth metrics
the moment they start working, without a rewrite, because the NBS team has committed to fixing them.

**Acceptance Criteria:**
- [ ] Utilization is resolved through one interface with two implementations: read
      `compute_vm_disk_ops_peak_quota_utilization` when it is present **and non-zero**; otherwise
      compute `ops_peak / spec.iops`
- [ ] The same fallback applies to bandwidth utilization via `..._bytes_peak_quota_utilization`
- [ ] Queue depth prefers an operations-denominated metric when one appears; the existing
      byte-denominated `write_queue_length` is used only as a fallback and is labelled as bytes so
      the two are never silently mixed
- [ ] Which source was used is visible in logs and exported metrics, so a silent switch is
      observable rather than mysterious
- [ ] Treating a zero `quota_utilization` as "unavailable" is deliberate and commented: today the
      metric is always zero, so zero cannot be distinguished from a genuinely idle disk. Once the
      fix ships, revisit — an idle disk will legitimately report 0
- [ ] Typecheck/lint passes

### US-006: Metric source — local `/proc/diskstats`
**Description:** As the decider in on-VM mode, I want local disk statistics because they are cheaper,
faster, and finer-grained than the API.

**Acceptance Criteria:**
- [ ] Reads `/proc/diskstats` at a configurable interval
- [ ] Derives ops/s, throughput, average queue depth, and service time per device
- [ ] Values are attributed to the correct cloud disk via the US-002 mapping
- [ ] No API quota is consumed for metrics in this mode
- [ ] Typecheck/lint passes

### US-007: Sliding-window evaluator
**Description:** As the decider, I want decisions based on a rolling window statistic so that a
single noisy sample cannot trigger a change.

**Acceptance Criteria:**
- [ ] Maintains a rolling window of samples per disk; window length configurable per direction
- [ ] Computes configurable percentiles over the window, not just means
- [ ] Refuses to emit a verdict until the window holds a minimum sample count
- [ ] Window state survives nothing — a restart starts a fresh window and scales nothing until full
- [ ] Typecheck/lint passes

### US-008: Scale-up decision
**Description:** As a user, I want the limit raised when the disk is genuinely saturated so that
spikes do not throttle.

**Acceptance Criteria:**
- [ ] Triggers when window utilization percentile ≥ threshold **and** a saturation signal confirms
      (latency quantile elevated or write queue non-empty)
- [ ] New value = `min(current + step, ceiling)`, rounded up to a 100-IOPS multiple
- [ ] Never exceeds `min(10000, 3000 × vcpuCount)` — the platform does not enforce this (FR-15c)
- [ ] Respects cooldown since the last change
- [ ] Typecheck/lint passes

### US-009: VM pool saturation guard
**Description:** As a user, I want the agent to recognise when more disk IOPS cannot help, so that it
does not spend money on the wrong lever.

**Acceptance Criteria:**
- [ ] Computes the VM pool as `3000 × spec.cpu.vcpuCount` from `GET /compute/v1/vmTypes/{vmType}`.
      The documented `spec.disks.totalIops` returns `null` on every VM type, so it cannot be used;
      re-check whether it starts being served before relying on the hardcoded ratio long-term
- [ ] Sums observed IOPS across all disks on the VM and compares to that pool size
- [ ] When the disk is saturated **and** the VM pool is saturated, no scale-up is issued
- [ ] Emits a distinct, actionable event: the bottleneck is vCPU, not disk IOPS
- [ ] The suppression is visible in logs and in the dry-run report
- [ ] Typecheck/lint passes

### US-010: Scale-down decision
**Description:** As a user, I want the limit lowered after sustained quiet so that I stop paying for
unused headroom.

**Acceptance Criteria:**
- [ ] Triggers when the **high percentile** (not mean) of window utilization ≤ threshold
- [ ] Blocked while any saturation signal is elevated
- [ ] New value = `max(current − step, floor)`, rounded down to a 100-IOPS multiple
- [ ] Never goes below 1000 IOPS
- [ ] Down-step and down-cooldown are independently configurable from up
- [ ] Typecheck/lint passes

### US-011: Applying the change
**Description:** As the agent, I need to write the new limit through the Compute API.

**Acceptance Criteria:**
- [ ] Uses the SDK's `DiskSugared.UpdateDisk` — it already targets
      `POST .../disks/{disk}?updateOnly=true`, so the agent never hand-rolls the HTTP call and can
      never accidentally create a disk
- [ ] Sends only `spec.iops`; every other field left unset (`optional.Optional` makes this explicit)
- [ ] Sets `IdempotencyKey` on every write so a retry after a mid-flight failure cannot double-apply
- [ ] Verifies the applied value from the response and reconciles against what was requested
- [ ] Failures are retried with backoff and never leave in-memory state claiming success
- [ ] Typecheck/lint passes

### US-012: Rate limiting and change budget
**Description:** As an operator, I want a hard cap on change frequency as a runaway guard.

**Acceptance Criteria:**
- [ ] Per-disk cooldown enforced separately for up and down
- [ ] Rolling 24 h change budget per disk; exhausting it logs a warning and blocks further changes
- [ ] Budget and cooldowns are configurable
- [ ] Typecheck/lint passes

### US-013: Dry-run mode
**Description:** As an operator, I want to see what the agent would do before letting it act.

**Acceptance Criteria:**
- [ ] `--dry-run` performs discovery, metric collection, and decisions but applies no change
- [ ] Each decision is sent with **`ValidateOnly=true`**, so the platform runs its real validation
      (bounds, 100-IOPS step, permissions) without mutating — a dry run that catches genuine
      rejections rather than only printing intent
- [ ] Prints each decision with the values that drove it, the resulting IOPS delta, and the
      platform's validation verdict
- [ ] Exits cleanly after a configurable number of evaluation cycles
- [ ] Typecheck/lint passes

### US-014: Observability
**Description:** As an operator, I want to understand what the agent did and why.

**Acceptance Criteria:**
- [ ] Structured logs for every decision, including the no-change ones and the reason
- [ ] Exposes its own Prometheus metrics: current/target IOPS per disk, decisions by outcome,
      API errors, window fill state
- [ ] Every applied change is logged at info level with before/after values
- [ ] Typecheck/lint passes

### US-015: Packaging
**Description:** As an operator, I want to install the agent in either mode without fighting
dependencies.

**Acceptance Criteria:**
- [ ] Single statically linked Go binary, no runtime dependencies in the guest
- [ ] systemd unit file provided
- [ ] `.deb` package build target
- [ ] Container image for fleet mode
- [ ] Typecheck/lint passes

---

### US-016: Least-privilege service account guidance
**Description:** As an operator, I want to know the minimum permissions the agent's service account
needs, per mode, so that I do not have to grant it broad rights.

**Acceptance Criteria:**
- [ ] README documents the minimum role set for `local` mode and for `fleet` mode separately
- [ ] Documents the read-only subset for running with `--dry-run`, so the agent can be evaluated
      before it is trusted to write
- [ ] Startup performs a permission preflight: verifies it can list disks, read the VM type, and
      (unless `--dry-run`) write IOPS — failing with a message naming the missing capability rather
      than a raw 403
- [ ] Documents that enabling the `compute` **service** in the project is a separate prerequisite
      from granting roles; a project without it returns 403 regardless of role
- [ ] Typecheck/lint passes

## 4. Functional Requirements

**Modes and scope**
- FR-1: The agent must support two run modes: `local` (manages disks of the VM it runs on) and
  `fleet` (manages disks of VMs listed in config).
- FR-2: In `local` mode the agent must discover its own VM and its attached disks automatically.
- FR-3: In `fleet` mode the target VMs must be listed explicitly in config.
- FR-4: Both modes must support three disk selection strategies: `all`, `all_except <list>`,
  `explicit <list>`.
- FR-5: Discovery must run continuously on an interval, so disks attached after startup enter
  management without a restart.

**Metrics**
- FR-6: In `local` mode the agent must prefer `/proc/diskstats` over the monitoring API.
- FR-7: In `fleet` mode the agent must use the MWS monitoring API.
- FR-8: The agent must not act on a disk whose metrics are missing or stale.

**Decisions**
- FR-9: All decisions must be computed over a sliding window using a configurable percentile,
  never a single sample.
- FR-10: Scale-up requires a utilization percentile above threshold **and** a confirming saturation
  signal (latency quantile or write queue depth).
- FR-11: Scale-down requires a **high** percentile of utilization below threshold and no elevated
  saturation signal.
- FR-12: The agent must never set IOPS outside `[1000, min(10000, 3000 × vcpuCount)]`, and always on
  a 100-IOPS boundary. `vcpuCount` comes from `GET /compute/v1/vmTypes/{vmType}` → `spec.cpu`;
  the documented `spec.disks.totalIops` is `null` on every type and cannot be used. The disk-level
  half (1000–10000, step 100) is enforced server-side, but the VM-pool half is not (FR-15c), so this
  check cannot be delegated to request validation.
- FR-13: The following must be configurable, with the defaults in §6: window length, percentile,
  utilization threshold, step, and cooldown — each independently for up and down; plus floor,
  ceiling override, discovery interval, and 24 h change budget.
- FR-14: When a disk is saturated and the VM pool (`3000 × vcpuCount`) is also saturated, the agent must
  suppress scale-up and emit a "vCPU is the bottleneck" event.

**Application and safety**
- FR-15: IOPS changes must go through the SDK's `UpdateDisk`, which carries `?updateOnly=true`; the
  agent must never create a disk. Every write must set an idempotency key.
- FR-15a: The platform enforces `1000 ≤ iops ≤ 10000` and divisibility by 100, rejecting violations
  with `400`. The agent must still clamp and round locally — to avoid wasting a round trip, and
  because the OpenAPI schema declares no bounds, so this validation is server behaviour rather than
  a contract.
- FR-15b: A disk **owned** by its VM (created inside the VM spec; `metadata.usages` contains `own`)
  cannot be modified through the disk API — the platform returns `403` naming the VM resource. This
  is an ownership constraint, not a permissions one: an independent disk attached by `ref` to the
  same VM edits successfully with the same credentials. The agent must classify disks (US-004a) and
  must not report this as a permissions failure.
- FR-15c: The VM pool ceiling (`3000 × vCPU`) is **not enforced by the platform**. A 2-vCPU VM with a
  6000 IOPS pool accepts a disk set to 10000 and bills for it; `validateOnly` does not catch it
  either. Enforcing the ceiling is entirely the agent's responsibility — FR-12 is the only thing
  standing between a bug and billed-but-unusable IOPS.
- FR-16: The agent must verify the applied value against the API response.
- FR-17: A per-disk cooldown and a rolling 24 h change budget must gate every change.
- FR-18: `--dry-run` must exercise the full decision path with no writes.
- FR-19: Every decision, including no-change, must be logged with its inputs.

**Permissions**
- FR-20: The agent must run with least privilege and must document, per mode, the minimum roles
  required. Roles confirmed to exist on the platform:

  | Role | Grants |
  |---|---|
  | `compute.viewer` | view VMs, disks, images, snapshots |
  | `compute.disk.user` | view disks and use them as a source |
  | `compute.editor` | view, create, delete, edit VMs and disks |
  | `monitoring.metricExporter` | export project metrics to external observability |
  | `compute.agent` | default role for the `compute` service agent |

  Proposed minimum sets, to be verified empirically before the README claims them:

  | Mode | Read-only (`--dry-run`) | Acting |
  |---|---|---|
  | `local` | `compute.viewer` | `compute.viewer` + `compute.editor` |
  | `fleet` | `compute.viewer` + `monitoring.metricExporter` | the same, plus `compute.editor` |

  `compute.editor` is broader than needed — it grants create and delete on VMs and disks, while the
  agent only ever updates `spec.iops` on an existing disk. If no narrower role covers disk update,
  that gap should be raised with the platform team rather than papered over.
- FR-21: The agent must preflight its permissions at startup and fail with a message naming the
  missing capability, not a raw 403.
- FR-23: Utilization and queue depth must be read through an abstraction with a documented
  preference order, so the platform's fixes can be adopted by configuration rather than a rewrite:
  1. `*_quota_utilization` when present and non-zero (committed by the NBS team, currently always 0)
  2. computed `ops_peak / spec.iops`
  Queue depth follows the same shape: operations-denominated metric first, byte-denominated
  `write_queue_length` as fallback, never mixed without an explicit unit label.
- FR-24: The active metric source must be observable — logged and exported — so a switch between
  platform-provided and computed values is visible.
- FR-22: Documentation must state that enabling the `compute` service in the project is a
  prerequisite distinct from role assignment — a project without the service enabled returns
  `403 PERMISSION_DENIED` no matter which roles are granted.

---

## 5. Non-Goals (Out of Scope)

- **Resizing VMs.** The agent detects that vCPU is the bottleneck and reports it; it never changes
  VM type or vCPU count.
- **Changing disk size.** Only the IOPS limit is managed. Disk size on this platform cannot shrink,
  which makes it a poor autoscaling target.
- **Local (NVMe) disks.** They use a different limit model (`50000 × namespace`) and are not
  NBS-PL2.
- **Modifying NBS, the hypervisor, or the monitoring metric set.** This is an external agent on
  public APIs.
- **Predictive or ML-driven scaling.** The dynamic range is only 10×; a reactive state machine is
  sufficient.
- **A UI.** Configuration is a file; observation is logs and Prometheus metrics.
- **Managing dependent disks in MVP.** They require a full read-modify-write through the VM object
  and are deferred (see OQ-3).

---

## 6. Default Configuration

All values are overridable. These ship as defaults.

| Parameter | Default | Rationale |
|---|---|---|
| `step_quantum` | 100 IOPS | matches the billing package size |
| `floor` | 1000 IOPS | included in the disk base price — free headroom |
| `ceiling` | `min(10000, 3000 × vcpuCount)` | computed; platform does not enforce it |
| `discovery_interval` | 5 min | picks up newly attached disks |
| **Scale-up** | | |
| `up.window` | 5 min | |
| `up.percentile` | p90 of utilization | tolerates isolated dips |
| `up.threshold` | ≥ 80% | |
| `up.confirm` | latency quantile elevated **or** write queue > 0 | utilization alone is ambiguous under sibling contention |
| `up.step` | `max(100, 20% of current)` | reach relief in a few steps |
| `up.cooldown` | 10 min | |
| **Scale-down** | | |
| `down.window` | 20 min | |
| `down.percentile` | **p95** of utilization | the safety property — see note |
| `down.threshold` | ≤ 40% | wide gap from 80% provides hysteresis |
| `down.step` | `max(100, 10% of current)` | descend at half the ascent rate |
| `down.cooldown` | 15 min | |
| **Guards** | | |
| `max_changes_per_24h` | 48 per disk | runaway guard, not a billing constraint |

**Why p95 on the way down.** The safety of scale-down comes from the *percentile*, not the window
length. Requiring the 95th percentile of utilization to sit below 40% means even the busiest moments
in the window were quiet — an average would let a real spike hide inside a calm mean. Because the
percentile carries the safety, the window can be 20 minutes instead of an hour without becoming
twitchy.

**Why the asymmetry.** Scaling up late costs performance during a spike; scaling down late costs
only money, and only briefly. Up is fast and decisive; down is slower and takes half-size steps.

---

## 7. Technical Considerations

- **Language and delivery:** single statically linked Go binary; systemd unit, `.deb`, and container
  image from one artifact. This mirrors `gpu-metrics-exporter`, which the MWS team already ships this
  way, so the packaging pipeline is a known quantity in-house.
- **Two credential types.** Reading metrics needs an **API key** with the `monitoring.metricExporter`
  role against `monitoring.mwsapis.ru`. Changing a disk needs an **IAM token** against
  `compute.mwsapis.ru`. In `local` mode the IAM token comes from the metadata service and must be
  refreshed on `expires_in` (~12 h in the docs example). These are different credentials on different
  hosts and must not be conflated.
- **Three hosts.** `api.mwsapis.ru` (SDK default), `compute.mwsapis.ru` (disk writes),
  `monitoring.mwsapis.ru` (metrics).
- **Disks on one VM are not independent.** The VM pool is split evenly across disks and the unused
  remainder is redistributed among the rest. A disk limit above its fair share is therefore useful —
  it is how one disk borrows the pool while siblings idle — but a limit above the whole pool is pure
  waste. Decisions must be computed with VM-level state in view, not per disk in isolation.
- **Billing is metered per minute at hourly rates.** Short-lived changes are charged for their actual
  duration, so shorter cooldowns are economically sound. Flapping still costs, which is what the
  hysteresis gap is for.
- **Guest device mapping is deterministic and API-served.** `StorageDiskStatusResponse.deviceName` is
  a required field; the guest path is `/dev/disk/by-id/mws-{deviceName}`, or `mws-{name}` when
  `deviceName` was not set at attach time. No serial-number probing, no heuristics. This removes the
  blocker that previously held back `local` mode.
- **Only `iops` and `size` are mutable on a disk.** Every other field in `DiskSpecRequest` is
  create-only and, per the spec, "may only be resent with its current value" on update. The agent
  therefore touches `spec.iops` and nothing else.

---

## 8. Success Metrics

- **Throttling reduction:** time spent at ≥ 95% of the disk IOPS limit falls by ≥ 50% versus a
  static limit on the same workload.
- **Cost:** average provisioned IOPS over a week is lower than the static limit that would have been
  needed to cover the same peaks, on the same workload.
- **Stability:** fewer than 5 changes per disk per day on a workload with a stable daily profile
  (evidence that hysteresis works).
- **Correctness:** zero scale-ups issued while the VM pool was saturated.
- **Safety:** zero IOPS values written outside `[1000, min(10000, 3000 × vcpuCount)]`; zero disks created.

---

## 9. Open Questions

**Closed since first draft:** guest device mapping (deterministic via `deviceName`, §7) and the
`DiskSpecRequest` schema with per-field mutability. `local` mode is no longer blocked.

- **OQ-2 — Metric names and labels in the Prometheus export.** The docs describe metrics in prose;
  no exported names are published. Also unresolved: the `<service>` path value for Compute, and
  whether a **read** queue metric exists (only write queue is documented). One request to
  `monitoring.mwsapis.ru` on a live project settles all three. **Blocks the decider.**
- **OQ-3 — Dependent disks.** Confirmed unmanageable through the disk API (FR-15b); they need a
  full-spec round trip through `vm update`. Deferred from MVP — confirm that is acceptable, given a
  VM's boot disk is always dependent and so always out of scope under this decision.
- **OQ-4 — Metric publication lag and granularity in the API.** The documented 15 s refers to console
  graphs. This sets the achievable floor on window length and cooldown.
- **OQ-5 — Platform rate limits on disk modification.** Undocumented. Determines whether the 24 h
  change budget needs to be lower.
- **OQ-6 — resolved.** The platform accepts and bills. See FR-15c.
- **OQ-7 — Latency thresholds for the confirming signal.** Absolute millisecond thresholds are
  workload-dependent. Options: a configurable absolute value, or a baseline-relative detector that
  learns the disk's quiet-state latency. Needs a decision.
