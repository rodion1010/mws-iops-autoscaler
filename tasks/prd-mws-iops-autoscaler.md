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
- [ ] Extracts per-disk: read/write op counts, read/write throughput, read/write latency quantiles,
      write queue depth
- [ ] Missing or stale metrics mark the disk as "no data" — the decider must not act on it
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
- [ ] Never exceeds `min(10000, vmType.spec.disks.totalIops)`
- [ ] Respects cooldown since the last change
- [ ] Typecheck/lint passes

### US-009: VM pool saturation guard
**Description:** As a user, I want the agent to recognise when more disk IOPS cannot help, so that it
does not spend money on the wrong lever.

**Acceptance Criteria:**
- [ ] Reads the VM pool size from `GET /compute/v1/vmTypes/{vmType}` → `spec.disks.totalIops`;
      never hardcodes `3000 × vCPU`
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
- [ ] `POST compute.mwsapis.ru/compute/v1/projects/<p>/disks/<d>?updateOnly=true` with body
      `{"spec": {"iops": N}}`
- [ ] `updateOnly=true` always set — the agent must never create a disk
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
- [ ] `--dry-run` performs discovery, metric collection, and decisions but issues no API writes
- [ ] Prints each decision with the values that drove it and the resulting IOPS delta
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
- FR-12: The agent must never set IOPS outside `[1000, min(10000, vmType.spec.disks.totalIops)]`,
  and always on a 100-IOPS boundary. The VM pool size must be read from the `vmTypes` API, not
  computed from a hardcoded per-vCPU ratio, so the agent tracks the platform if the ratio changes.
  The OpenAPI schema places no bounds on `iops` (`int64`, no min/max), so this check is the agent's
  responsibility and cannot be delegated to request validation.
- FR-13: The following must be configurable, with the defaults in §6: window length, percentile,
  utilization threshold, step, and cooldown — each independently for up and down; plus floor,
  ceiling override, discovery interval, and 24 h change budget.
- FR-14: When a disk is saturated and the VM pool (`vmType.spec.disks.totalIops`) is also saturated, the agent must
  suppress scale-up and emit a "vCPU is the bottleneck" event.

**Application and safety**
- FR-15: IOPS changes must be applied with `?updateOnly=true`; the agent must never create a disk.
- FR-16: The agent must verify the applied value against the API response.
- FR-17: A per-disk cooldown and a rolling 24 h change budget must gate every change.
- FR-18: `--dry-run` must exercise the full decision path with no writes.
- FR-19: Every decision, including no-change, must be logged with its inputs.

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
| `ceiling` | `min(10000, vmType.spec.disks.totalIops)` | read from the API, not configured |
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
- **Safety:** zero IOPS values written outside `[1000, min(10000, vmType.spec.disks.totalIops)]`; zero disks created.

---

## 9. Open Questions

**Closed since first draft:** guest device mapping (deterministic via `deviceName`, §7) and the
`DiskSpecRequest` schema with per-field mutability. `local` mode is no longer blocked.

- **OQ-2 — Metric names and labels in the Prometheus export.** The docs describe metrics in prose;
  no exported names are published. Also unresolved: the `<service>` path value for Compute, and
  whether a **read** queue metric exists (only write queue is documented). One request to
  `monitoring.mwsapis.ru` on a live project settles all three. **Blocks the decider.**
- **OQ-3 — Dependent disks.** Changing their IOPS requires a full spec through
  `mws compute vm update`, a different code path from independent disks. Deferred from MVP —
  confirm that is acceptable.
- **OQ-4 — Metric publication lag and granularity in the API.** The documented 15 s refers to console
  graphs. This sets the achievable floor on window length and cooldown.
- **OQ-5 — Platform rate limits on disk modification.** Undocumented. Determines whether the 24 h
  change budget needs to be lower.
- **OQ-6 — Behaviour when `iops` exceeds the VM ceiling.** Reject, silent clamp, or accept and bill?
  Decides whether the FR-12 check must be strictly pre-emptive.
- **OQ-7 — Latency thresholds for the confirming signal.** Absolute millisecond thresholds are
  workload-dependent. Options: a configurable absolute value, or a baseline-relative detector that
  learns the disk's quiet-state latency. Needs a decision.
