# PRD: MWS IOPS Autoscaler

Status: draft · Last updated: 2026-08-28
Platform facts referenced here live in `CLAUDE.md` at the repo root.

> **This PRD is the primary document.** It states *what* is being built and *why*. `docs/design.md`
> and `docs/adr/` are derived from it and answer *how*; `docs/configuration.md` follows from both.
> A requirement exists once it is written here — no design document introduces, widens or drops one
> on its own.
>
> **Sync rule.** Any change is written into this PRD, `docs/design.md` and `docs/adr/` in the same
> change — always, without being asked. This holds even when the change is discovered from below: if
> a measured platform fact invalidates a requirement, this PRD is amended first and an ADR records
> the reasoning. A PRD that contradicts the design is worse than no PRD: it reads as current and is
> not. See `docs/README.md` for the full rule.

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
- [ ] Config entries are matched to discovered disks by `device_name` in the unprefixed form
      (`datadisk`, not `mws-datadisk`); the agent applies the `mws-` prefix itself when resolving
      the guest path and when matching the metric label (ADR-004)
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
- [ ] Classifies from the VM's `spec.storage.disks[]`: `disk.ref` → **independent**,
      `disk.spec` → **dependent**. (`metadata.usages` containing `own` gives the same answer when
      reading a disk directly. `managed:` is not usable — it exists only in the UI-facing API.)
- [ ] **Both classes are managed.** Independent disks are written through `UpdateDisk`; dependent
      disks through `vm update --storage-disks` (US-011a)
- [ ] The class determines only the write path, never whether a disk is in scope
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
- [ ] **Disks are named by `device_name` in both modes, never by kernel device path** (`/dev/vdb` is
      assigned in discovery order and is not stable across reboots or re-attachment — a policy would
      silently land on the wrong disk). Written unprefixed; `(instance, device_name)` is the key in
      fleet mode, where selectors nest under their instance. See ADR-004
- [ ] `--validate-config` rejects a kernel device path, a prefixed `mws-` name, a duplicate
      `device_name` within one VM's list, and a `device_name` matching no discovered disk
- [ ] Strategy is settable globally and overridable per VM
- [ ] Continuous rediscovery interval is configurable; new disks enter management without restart
- [ ] Typecheck/lint passes

### US-005: Metric source — platform monitoring API
**Description:** As the decider, I need disk metrics from the MWS monitoring API. `fleet` mode
requires this source exclusively (no guest access); `local` mode can opt into it too (ADR-001), which
is how it gets validated before `fleet` depends on it with no fallback (US-005b).

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

### US-005b: Metric source comparison (shadow) mode
**Description:** As an operator, I want `local` mode to run platform metrics alongside guest metrics,
with the platform side never driving actuation, so that I can see how much the two sources disagree
on real load before `fleet` mode has to depend on the platform source with no guest fallback at all.

**Acceptance Criteria:**
- [ ] `local` mode can enable `GuestSource` and `PlatformSource` concurrently; `GuestSource` remains
      the only one eligible for actuation regardless of this mode's setting
- [ ] Each source keeps its own window state and latency baseline — the two are not portable
      (ADR-001) — and is analyzed and decided independently
- [ ] Both `Decision`s are recorded in the decision journal every cycle, tagged by source, whether or
      not actuation is enabled
- [ ] Divergence — a different `Action`, or a `Target` differing by more than a configurable
      threshold — is surfaced as its own log line and metric, not left for a human to diff by hand
- [ ] Comparison mode is the one case where `local` mode needs the `monitoring.metricExporter` role;
      documented against US-016
- [ ] Typecheck/lint passes

### US-006: Metric source — local `/proc/diskstats`
**Description:** As the decider in `local` mode, I want guest statistics as the **primary** source,
because they need no metrics credential, cross no network, and carry signals the platform lacks.

**Acceptance Criteria:**
- [ ] Guest is the **default** source in `local` mode; the platform source is opt-in (ADR-001)
- [ ] Reads `/proc/diskstats` at a configurable interval
- [ ] Derives ops/s and throughput **split by read and write**, plus queue depth in operations (a
      single combined value — `/proc/diskstats`' time-in-queue field is not split by direction at
      the kernel level, so this is one number covering both, still an improvement over the
      platform's byte-denominated one) and average service time per device — none of which the
      platform exposes
- [ ] Detects throttling directly: served ≈ provisioned limit **and** queue sustained above zero
- [ ] Values are attributed to the correct cloud disk via the US-002 mapping
- [ ] Requires **no** monitoring API key and no `monitoring.metricExporter` role
- [ ] VM-level pool data still comes from the Compute API, on the slow path only
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
- [ ] Utilization is a **gate, not a trigger**: at or above `saturation.utilization_threshold` on
      **either IOPS or bandwidth utilization** (ADR-008 — NBS-PL2's fixed IOPS-to-bandwidth ratio
      means a large-block workload saturates bandwidth well before it saturates IOPS count, so
      gating on IOPS alone would miss it) the disk *may* be starved; below both, it cannot be, and
      the signal checks are skipped
- [ ] Triggers only on confirmed saturation — queue depth at/above threshold, **or** p95 latency
      above the disk's own baseline × `baseline_multiplier`, **or** throttling detected — sustained
      over `scale_up_window`
- [ ] A disk at 100% utilization with flat latency and an empty queue is efficient and **must not**
      be scaled up
- [ ] Target is computed from estimated demand: `demand / target.scale_up_utilization`, not by
      stepping a fixed percentage
- [ ] Rounded to a 100-IOPS multiple and clamped by `max_scale_up_ratio`
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
- [ ] Triggers on sustained low demand over `scale_down_window`, with every saturation signal quiet:
      queue low, latency at baseline, no throttling
- [ ] Target is computed as `p95_demand / target.scale_down_utilization`, deliberately leaving
      headroom so the new limit does not sit flush against current demand
- [ ] Rounded to a 100-IOPS multiple and clamped by `max_scale_down_ratio`
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

### US-011a: Applying the change — dependent disks
**Description:** As the agent, I need to change IOPS on a VM-owned disk, which the disk API refuses.

**Acceptance Criteria:**
- [ ] Reads the VM, capturing `etag`
- [ ] Mutates exactly one entry in `spec.storage.disks[]`, resending `diskType` and `size` unchanged
- [ ] Asserts every other field is byte-identical to what was read; aborts the write if not
- [ ] Sends `etag` and an idempotency key; an etag conflict aborts and retries next cycle, never forces
- [ ] Verifies the applied value from the response
- [ ] Covered by a concurrent-modification test
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
- [ ] **Independent disks:** each decision is sent with `ValidateOnly=true` (`UpdateDisk` supports
      it), so the platform runs its real validation (bounds, 100-IOPS step, permissions) without
      mutating — a dry run that catches genuine rejections rather than only printing intent
- [ ] **Dependent disks:** no equivalent exists — `UpdateVirtualMachine` has no `validateOnly` (ADR-007).
      The report states the computed decision and the agent's own client-side checks (range, step,
      VM pool guard), and is labelled as such — not presented as a server-confirmed verdict
- [ ] Prints each decision with the values that drove it, the resulting IOPS delta, and — per disk —
      either the platform's validation verdict (independent) or "computed, not server-checked"
      (dependent)
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

### US-015: Packaging — binary
**Description:** As an operator, I want to install the agent on a VM without fighting dependencies.

**Acceptance Criteria:**
- [ ] Single statically linked Go binary, no runtime dependencies in the guest
- [ ] systemd unit file provided
- [ ] `.deb` package build target
- [ ] Typecheck/lint passes

### US-015a: Packaging — container image
**Description:** As an operator running `fleet` mode, I want a container image, because a central
controller is naturally run that way rather than installed on a host.

**Acceptance Criteria:**
- [ ] Container image, built from the same static binary as US-015
- [ ] Documented deployment example against the test environment (`CLAUDE.md`, "Test environment")
- [ ] Typecheck/lint passes

---

### US-016: Least-privilege service account guidance
**Description:** As an operator, I want to know the minimum permissions the agent's service account
needs, per mode, so that I do not have to grant it broad rights.

**Acceptance Criteria:**
- [ ] README documents the minimum role set for `local` mode and for `fleet` mode separately, and
      notes that `local` mode needs the `monitoring.metricExporter` role only when comparison mode
      (US-005b) is enabled — otherwise it needs no metrics credential at all
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
- FR-10: Scale-up requires confirmed **saturation**. Utilization at/above the gate is a
  precondition; the trigger is queue depth, latency above the disk's own baseline, or throttling.
  Utilization alone must never cause a scale-up (ADR-002).
- FR-11: Scale-down requires sustained low demand with every saturation signal quiet. Targets on
  both directions are computed as `demand / target_utilization`, never as a fixed percentage step.
- FR-12: The agent must never set IOPS outside `[1000, min(10000, 3000 × vcpuCount)]`, and always on
  a 100-IOPS boundary. `vcpuCount` comes from `GET /compute/v1/vmTypes/{vmType}` → `spec.cpu`;
  the documented `spec.disks.totalIops` is `null` on every type and cannot be used. The disk-level
  half (1000–10000, step 100) is enforced server-side, but the VM-pool half is not (FR-15c), so this
  check cannot be delegated to request validation.
- FR-13: The following must be configurable, with the defaults in §7: window length, percentile,
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
- FR-15d: Dependent disks are written through `vm update --storage-disks`: read the VM, mutate one
  entry, resend with `etag` and an idempotency key, and assert every other field round-trips
  unchanged. Independent disks use the single-field disk path (FR-15).
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

---

## 6. Release Phases

The User Stories in §3 are not one delivery — they group into five phases. Each phase is shippable
and independently useful; nothing here is "half a feature." `docs/design.md` §9.1 maps each phase
onto its engineering milestones (`M`); the ordering rationale — **local mode ships complete before
fleet mode is built at all** — is recorded in ADR-005.

**Rule:** a story moves between phases only by editing this table — not by quietly slipping scope
into whichever phase is being coded at the time.

### Phase 1 — MVP: local mode, scale-up, binary

**Goal:** a genuinely useful agent on day one — it discovers its own VM's disks and actually raises
their IOPS limit when they saturate. Scale-up only: it is the direction with bounded blast radius
(it can only add capacity, and any overspend is visible and reversible on the very next cycle), so
this phase does not need a separate observe-only release to earn trust — `--dry-run` (US-013) stays
available as the validation step an operator takes before flipping to live, folded into this phase
rather than gating a phase of its own.

| Story | What it delivers |
|---|---|
| US-001 | Config file, parsed and validated |
| US-002 | On-VM disk discovery (`local` mode) |
| US-004a | Classify each disk dependent / independent |
| US-004 | `all` / `all_except` / `explicit` disk selection |
| US-006 | Guest metric source (`/proc/diskstats`) |
| US-007 | Sliding-window evaluator |
| US-008 | Scale-up decision |
| US-009 | VM pool saturation guard |
| US-011 | Apply the change — independent disks |
| US-011a | Apply the change — dependent disks |
| US-012 | Rate limiting and change budget |
| US-013 | Dry-run mode |
| US-014 | Observability: decision journal, metrics, logs |
| US-016 | Least-privilege service-account guidance |
| US-015 | Binary, systemd unit, `.deb` |

**Out:** scale-down, platform metrics, fleet mode, container image.

**Exit criterion:** `local`-mode E2E green with real scale-up writes; soak holds budget/cooldown
under sustained load; installs from a binary or `.deb`, no source checkout. = design.md M0–M6.

### Phase 2 — Platform metrics, validated in local mode

**Goal:** build the metric source `fleet` mode will later depend on exclusively, and prove it against
real load while a trusted fallback (guest) is still right there to check it against.

| Story | What it delivers |
|---|---|
| US-005 | Platform monitoring metric source |
| US-005a | Adopt improved platform metrics as the NBS team ships them |
| US-005b | Comparison (shadow) mode — guest and platform run concurrently, only guest ever actuates, both decisions are journaled, divergence surfaced |

**Exit criterion:** comparison mode running against the live test environment; divergence between
guest and platform decisions is understood, not just logged. = design.md M7–M8.

### Phase 3 — Bidirectional (local mode)

**Goal:** close the loop in the environment that is cheapest to get right — scale down too.
Deferred to its own phase because its failure mode is the real one: a wrong scale-down throttles the
workload, unlike a wrong scale-up which only overspends.

| Story | What it delivers |
|---|---|
| US-010 | Scale-down decision |
| — | The default parameter set's thresholds calibrated from Phase 1–2 telemetry (design.md §8.5 calibration harness) |

**Exit criterion:** soak and chaos tests green with scale-down enabled, `local` mode. = design.md M9.

### Phase 4 — Fleet mode, container

**Goal:** extend the now-proven bidirectional logic and the now-validated platform metric source to
many VMs from one place. Nothing decision-related is new here — only discovery scope, config
inheritance, and the deployment shape.

| Story | What it delivers |
|---|---|
| US-003 | Fleet disk discovery across named VMs |
| — | Project → VM → disk policy inheritance (`docs/configuration.md` §5) |
| US-015a | Container image |

**Exit criterion:** fleet E2E green, bidirectional from day one, deployable as a container.
= design.md M10.

### Phase 5 — Further fleet capabilities

**Goal:** not yet broken into stories — deliberately left open rather than scheduled, per
`docs/TODO.md`'s own rule. Likely candidates: fleet-scale soak/chaos, multi-project scheduling,
cost/budget reporting across a fleet, and reintroducing named presets (ADR-006) once there is a
reason to choose between tunings rather than one validated default. = design.md M11.

---

## 7. Default Configuration

The user-facing surface is limits plus disk selection. Controller internals are `advanced` and
opt-in. Phase 1 ships one built-in default parameter set — no preset name to choose (ADR-006). Full
model, default values and examples: `docs/configuration.md`.

| Parameter | Default | Rationale |
|---|---|---|
| `limits.min_iops` | 1000 | included in the disk base price; lower is impossible |
| `limits.max_iops` | 10000 | clamped further to `3000 × vcpuCount` at runtime |
| `billing_step_iops` | 100 | platform-fixed, not user-settable |
| `discovery_interval` | 5 min | picks up newly attached disks |
| **Saturation (gate + signals)** | | |
| `utilization_threshold` | 0.98 | **gate only**, on whichever of IOPS/bandwidth utilization is higher (ADR-008) — never triggers a change by itself |
| `queue_depth_threshold` | 1 | units differ by source: ops in `local`, bytes in `fleet` |
| `latency.mode` | `relative` | absolute thresholds are wrong for every workload |
| `latency.baseline_multiplier` | 1.5 | versus the disk's own observed baseline |
| **Target sizing** | | |
| `target.scale_up_utilization` | 0.85 | `target = demand / 0.85` after saturation is confirmed |
| `target.scale_down_utilization` | 0.70 | leaves headroom instead of landing flush on demand |
| **Windows and cooldowns** | | |
| `scale_up_window` | 5 min | above the ~135 s platform detection floor |
| `scale_down_window` | 30 min | down is deliberately slower than up |
| `cooldown.scale_up` | 5 min | |
| `cooldown.scale_down` | 30 min | |
| **Step limits** | | |
| `min_change_iops` | 200 | suppresses churn worth one or two billing steps |
| `max_scale_up_ratio` | 2.0 | also bounds a pathological demand estimate |
| `max_scale_down_ratio` | 0.5 | |
| `max_changes_per_24h` | 48 per disk | runaway guard; metering is per-minute, so not a cost constraint |

**Why utilization is only a gate.** With provisioned billing the limit is already paid for, so a disk
using all of it is efficient, not distressed. Scaling on utilization systematically over-provisions
exactly the workloads that are behaving well. The trigger is unmet demand — queueing, latency above
the disk's own baseline, throttling. See ADR-002.

**Why latency is relative.** `p95 > 10 ms` is wrong for everyone; acceptable latency is
workload-specific. Comparing against each disk's own baseline adapts automatically, at the cost of a
warm-up period before the agent may act.

**Why the asymmetry.** Scaling up late costs performance during a spike; scaling down late costs a
little money briefly. Up is fast and decisive; down is slow and leaves headroom.

> **These are design defaults, not validated values.** They must be calibrated from observe-only
> telemetry (design §8.5) before GA.

---

## 8. Technical Considerations

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
  blocker that previously held back `local` mode. `deviceName` is therefore also the identifier the
  operator writes in config — unprefixed — since it is the one value shared by config, guest path and
  metric label (ADR-004).
- **Only `iops` and `size` are mutable on a disk.** Every other field in `DiskSpecRequest` is
  create-only and, per the spec, "may only be resent with its current value" on update. The agent
  therefore touches `spec.iops` and nothing else.

---

## 9. Success Metrics

- **Saturation reduction:** time spent in a confirmed-saturated state (queueing or latency above
  baseline while at the limit) falls by ≥ 50% versus a static limit on the same workload. Measured
  on saturation, not utilization — high utilization is not a defect.
- **Cost:** average provisioned IOPS over a week is lower than the static limit that would have been
  needed to cover the same peaks, on the same workload.
- **Stability:** fewer than 5 changes per disk per day on a workload with a stable daily profile
  (evidence that hysteresis works).
- **Correctness:** zero scale-ups issued while the VM pool was saturated; zero scale-ups issued on
  a disk at high utilization with no saturation signal.
- **Safety:** zero IOPS values written outside `[1000, min(10000, 3000 × vcpuCount)]`; zero disks created.

---

## 10. Open Questions

**Closed since first draft:** guest device mapping (deterministic via `deviceName`, §8) and the
`DiskSpecRequest` schema with per-field mutability. `local` mode is no longer blocked.

- **OQ-2 — resolved.** Measured against a live project. `<service>` = `compute`; the full metric
  name and label set is recorded in `CLAUDE.md` ("Prometheus export — measured"). No read queue
  metric exists — only `compute_vm_disk_write_queue_length`; read-heavy saturation in `fleet` rests
  on latency alone (design.md §10, item 4).
- **OQ-3 — resolved.** Dependent disks are supported via the VM write path (ADR-003, US-011a). They
  are not excluded, and boot disks are therefore in scope. A dependent disk can also be converted to
  independent (`spec-to-ref`), but that is one-way and changes deletion semantics, so the agent only
  ever *recommends* it.
- **OQ-4 — resolved.** Measured against a live project (`CLAUDE.md`, "Timing, measured"):
  publication every 30 s, a 60 s trailing averaging window, 62–90 s publication lag (mean 75 s).
  Worst case detection latency ≈ 135 s — the hard floor on window length; the 15 s figure in the docs
  is the console refresh, not the API's granularity. All configured windows (§7) already clear this
  floor.
- **OQ-5 — Platform rate limits on disk modification.** Undocumented. Determines whether the 24 h
  change budget needs to be lower.
- **OQ-6 — resolved.** The platform accepts and bills. See FR-15c.
- **OQ-7 — Latency thresholds for the confirming signal.** Absolute millisecond thresholds are
  workload-dependent. Options: a configurable absolute value, or a baseline-relative detector that
  learns the disk's quiet-state latency. Needs a decision.
