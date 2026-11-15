# MWS IOPS Autoscaler — Configuration

Derived from `docs/prd-mws-iops-autoscaler.md` (the primary document) and `docs/design.md`, with the
platform constraints from `docs/platform-reference.md` folded in. The user-facing surface described here follows the
PRD's requirements — it does not define them.

> **All numeric defaults below are design defaults, not production-validated values.** They must be
> calibrated against observe-only telemetry (design §8.5) before GA.

---

## 1. Principles

**One validated default, not a choice of knobs.** Phase 1 ships a single built-in parameter set
(§3) — there is no preset name to pick; named presets are deferred (`docs/TODO.md`). The
user-facing surface is limits plus disk selection. Controller internals stay adjustable
(`advanced`) without breaking anyone's config.

**Inheritance, not repetition.** Policy is declared once and overridden where it differs.

**v1 user-facing fields:**

```yaml
limits.min_iops
limits.max_iops
disks (selection)
```

Everything under `advanced` is experimental: its fields may change before v1, until telemetry
justifies freezing them. Setting a field under `advanced` is enough to apply it.

---

## 2. Policy model

```yaml
policy:
  limits:
    min_iops: 1000           # never below platform min 1000
    max_iops: 10000          # never above platform max 10000

  advanced:
    evaluation:
      # No collection_interval here — sampling cadence is process-wide,
      # set once by runtime.metrics.collection_interval below (design.md §4.2).
      scale_up_window: 5m
      scale_down_window: 30m

    saturation:
      utilization_threshold: 0.98        # GATE, not a trigger — IOPS or bandwidth, whichever is higher (design.md §4.4)
      queue_depth_threshold: 1
      latency:
        mode: relative                   # only "relative" is accepted — "absolute" is rejected by validation (§8, issue #6)
        baseline_multiplier: 1.5

    target:
      scale_up_utilization: 0.85
      scale_down_utilization: 0.70

    cooldown:
      scale_up: 5m
      scale_down: 30m

    step:
      billing_step_iops: 100             # platform-fixed; not user-settable
      min_change_iops: 200
      max_scale_up_ratio: 2.0
      max_scale_down_ratio: 0.5

    budget:
      max_changes_per_24h: 48
```

### Field notes

| Field | Meaning |
|---|---|
| `limits.min_iops` | Floor. 1000 is included in the disk's base price, so going lower is impossible and would save nothing. |
| `limits.max_iops` | User ceiling. Clamped to 10000 and further to `3000 × vCPU` at runtime — **the platform does not enforce the VM cap**, the agent does. |
| `saturation.utilization_threshold` | Precondition only, checked against **whichever of IOPS or bandwidth utilization is higher** (design.md §4.4) — a disk below it on both cannot be limit-starved, so signal checks are skipped. Never triggers a change on its own. |
| `saturation.queue_depth_threshold` | Units differ by source: operations in `local`, bytes in `fleet`. The agent will not compare across units — the Analyzer rejects a window whose samples are not all `local`'s own ops unit before evaluating this threshold or summing queue growth into an IOPS demand figure (issue #18), rather than silently comparing a bytes reading against an ops-calibrated number. |
| `saturation.latency.baseline_multiplier` | Relative to the disk's own baseline. An absolute threshold is wrong for everyone; acceptable latency is workload-specific. |
| `target.scale_up_utilization` | Applied to estimated demand *after* saturation is confirmed: `target = demand / 0.85`. |
| `target.scale_down_utilization` | Deliberately lower, so scale-down leaves headroom instead of landing flush against current demand. |
| `step.min_change_iops` | Suppresses churn worth one or two billing steps. |
| `step.max_scale_up_ratio` | Caps a single increase. Also bounds a pathological demand estimate. |
| `budget.max_changes_per_24h` | Runaway guard. Not a cost constraint — metering is per-minute, so frequent adjustment is not inherently wasteful. |

---

## 3. Default parameter set

Phase 1's one built-in `advanced` value set — what `policy.advanced` resolves to when nothing
overrides it (configuration.md §1):

```yaml
evaluation:  {scale_up_window: 5m,  scale_down_window: 30m}
saturation:  {utilization_threshold: 0.98, queue_depth_threshold: 1,
              latency: {mode: relative, baseline_multiplier: 1.5}}
target:      {scale_up_utilization: 0.85, scale_down_utilization: 0.70}
cooldown:    {scale_up: 5m,  scale_down: 30m}
step:        {min_change_iops: 200, max_scale_up_ratio: 2.0, max_scale_down_ratio: 0.5}
budget:      {max_changes_per_24h: 48}
```

This is not user-facing syntax. An operator overrides individual fields through `policy.advanced`
or a disk's own `advanced` block.

---

## 4. Disk selection

### 4.1 How a disk is named

**Disks are addressed by `device_name`, in both modes.** Write the exact platform-visible value: what
the platform reports in `status.storage.disks[].deviceName`, puts on the `device_name` label of every
exported metric, and uses for the guest path — copy it from any of those three:

```yaml
disks:
  policies:
    mws-datadisk: {}          # correct — the platform-visible form
```

`mws-datadisk` here comes from an attach-time `deviceName: datadisk`; the platform always adds the
`mws-` prefix on the way out (`status.storage.disks[].deviceName`, the `device_name` metric label, and
the guest path `/dev/disk/by-id/mws-datadisk` all carry it). If the attach-time `deviceName` itself
already started with `mws-` (the platform's schema permits this), its platform-visible form doubles
the prefix — `deviceName: mws-data` is visible as `mws-mws-data`, and that is the value to write here.

The unprefixed form (`datadisk`) is rejected by `--validate-config`: it cannot be the
exact value VM status reports, since the platform always adds `mws-` on the way out.

**Kernel device names (`/dev/vdb`, `/dev/sdb`) must never be used.** They are assigned in discovery
order, which is not stable across reboots or attach/detach cycles: `/dev/vdb` today can be `/dev/vdc`
tomorrow, and the agent would silently apply one disk's policy to another — a config that looks
correct and is wrong. See design.md §4.1.

**A policy follows the device name, not the disk.** If you detach `mws-wal` and attach a different
disk as `mws-wal`, the new disk gets the old one's policy. If you move a disk to another VM, its
policy stays behind in this VM's config.

`deviceName` is unique **within a VM**, not globally. In `local` mode that is sufficient. In `fleet`
mode disk selectors are always nested under their instance (§6.3), so the effective key is the pair
`(instance, device_name)`.

### 4.2 Strategies

Two modes, plus an optional per-disk policy in either:

```yaml
disks:
  mode: auto-discovery          # every discovered manageable disk
```

```yaml
disks:
  mode: auto-discovery          # everything except these
  exclude:
    - mws-bootdisk
```

```yaml
disks:
  mode: auto-discovery          # everything, one disk with its own policy
  policies:
    mws-wal:
      limits: {min_iops: 5000}
```

```yaml
disks:
  mode: selected                # only these, each with an optional policy
  policies:
    mws-datadisk: {}
    mws-logdisk:
      limits: {min_iops: 3000, max_iops: 10000}
```

`exclude` is a plain list of device names. `policies` maps a device name to a policy override (§5);
`{}` means no override. In `mode: selected` its keys are the managed set; in `mode: auto-discovery`
they only override the policy of disks that are managed anyway. `exclude` exists only in
`mode: auto-discovery`, and a disk cannot be both excluded and given a policy.

**`mode: selected` is exhaustive, not best-effort (issue #5, reopened).** Every device name listed is
the operator's stated intent, not a best-effort filter — silently managing fewer disks than named would
make the agent's actual behavior diverge from what was configured, with no signal beyond a log line.
What happens when one doesn't resolve depends on discovery history, not one fixed rule:

- **On the first discovery attempt of the process's life** (cold start), an unresolved `device_name` is
  a startup error: nothing is managed until it's resolved — either the missing disk is attached, or the
  config is corrected — and `/readyz` reports not ready. Managing a silently partial set with no prior
  successful discovery to fall back to would diverge from configured intent with nothing to show for
  it.
- **Once at least one discovery has already succeeded** (runtime — a disk detaches, say), only the
  unresolved name degrades: every other selected disk keeps being managed normally. The degraded name
  appears in `/readyz`'s body as `configured_disks_unmanaged` (`{device_name, reason}`) and as the
  `iops_autoscaler_disk_configured_unmanaged{device_name,reason}` gauge — neither of which flips
  `/readyz`'s binary verdict, which stays reserved for whole-agent failures. There is no escalation
  timer back to fatal; the alerting contract for the exposed signal is in docs/design.md §7.3.

`mode: auto-discovery`'s `exclude` gets the identical treatment for the same reason, not the asymmetric
"harmless no-op" it used to be: an exclude entry that never matches a live disk could just as easily be
a typo that leaves a disk the operator meant to exclude managed anyway, and the two cases are otherwise
indistinguishable. A cold-start mismatch there is fatal the same way; a runtime one degrades the same
way, only the reported `reason` differs (`exclude_target_not_found` rather than `selected_not_found`
— see docs/design.md §4.1). A `policies` key in `mode: auto-discovery` that matches no live disk is
handled the same way too, with `reason` `policy_target_not_found`: a typo would otherwise leave the
intended disk on the VM-wide policy.

In `fleet` mode an `exclude` above the instance level will be best-effort: one shared list cannot
expect every VM to carry every listed disk.

Rediscovery runs on an interval, so disks attached after startup are picked up without a restart —
including recovering from either kind of mismatch once the VM's inventory becomes consistent with the
configured list again, at which point the entry disappears from both `/readyz` and the gauge rather
than merely reading resolved.

**Classification is automatic and not user-configurable.** Each disk is dependent (VM-owned, written
through the VM spec) or independent (written through the disk API) — see design.md §4.7. Both are managed;
the distinction only changes the write path.

---

## 5. Inheritance

**Fleet:**

```
disk.policy → vm.policy → project.policy → platform default
```

**Local** (no project layer):

```
disk.policy → vm.policy → platform default
```

An object inherits the whole policy, then overrides only the fields it sets explicitly.

**`limits.min_iops` is always a per-disk floor, never a VM- or project-aggregate minimum.** The
platform bills and enforces the 1000-IOPS minimum per disk, not as a pool shared across a VM's disks —
inheritance gives each disk its own resolved `min_iops` (disk override, else `vm.policy`, else platform
default), the same number a `fleet`-mode VM- or project-level override would also resolve to for any
disk that doesn't set its own. There is no mechanism, and none is planned, for a VM- or project-level
`min_iops` to mean "at least this much, summed across the VM's disks" — an aggregate reading would just
need dividing among disks somehow, which reduces to the per-disk override this schema already has.

**`max_iops` is a limit for each disk, not for the VM.** With `policy.limits.max_iops: 5000` and three
disks, each disk can go up to 5000, so together up to 15000. The VM-wide ceiling is the platform's
`3000 × vCPU` pool (§7).

**Without `policy.limits`, a disk can go anywhere from 1000 to 10000 IOPS**, the platform's full range.
Set `max_iops` to cap what the agent may spend, and run with `-dry-run` first to see what it would do.

---

## 6. Examples

### 6.1 Local, auto-discovery

```yaml
version: 1
mode: local

policy:
  limits: {min_iops: 1000, max_iops: 10000}

disks:
  mode: auto-discovery
  exclude:
    - mws-bootdisk

runtime:
  metrics:
    source: guest              # default; 'platform' is not implemented yet
    collection_interval: 10s
  controller:
    reconcile_interval: 30s
    discovery_interval: 5m
```

### 6.2 Local, per-disk policies

```yaml
version: 1
mode: local

policy:
  limits: {min_iops: 1000, max_iops: 10000}

disks:
  mode: selected
  policies:
    mws-datadisk: {}

    mws-logdisk:
      limits: {min_iops: 3000, max_iops: 10000}

    mws-backupdisk:
      limits: {min_iops: 1000, max_iops: 6000}
      advanced:
        step: {min_change_iops: 300}   # cost-sensitive disk: fewer, larger changes

runtime:
  metrics:   {source: guest, collection_interval: 10s}
  controller: {reconcile_interval: 30s, discovery_interval: 5m}
```

### 6.3 Fleet, project → VM → disk

```yaml
version: 1
mode: fleet

project:
  policy:
    limits: {min_iops: 1000, max_iops: 10000}

  instances:
    discovery:
      mode: all

    overrides:
      - instance: database-01
        policy: {limits: {min_iops: 3000}}
        disks:
          mode: selected
          policies:
            mws-db-data: {}
            mws-db-wal:
              limits: {min_iops: 5000, max_iops: 10000}

      - instance: worker-01
        disks:
          mode: auto-discovery
          exclude: [mws-bootdisk]

      - instance: batch-01
        policy:
          limits: {max_iops: 5000}

runtime:
  metrics:
    source: platform           # only option in fleet
    collection_interval: 30s   # matches the platform publication interval
  controller:
    reconcile_interval: 30s
    discovery_interval: 5m
```

---

## 7. Guardrails the user cannot override

Regardless of the default parameter set or any override:

- disk IOPS min (1000) and max (10000) — enforced by the platform, and again by the agent
- **VM aggregate cap `3000 × vCPU`** — *not* enforced by the platform; the agent is the only guard
- billing step of 100 IOPS
- cooldowns between resizes
- minimum meaningful change
- protection against concurrent resize of the same disk

---

## 8. Validation rules

`--validate-config` must reject, with a message naming the field:

- `version` missing or anything other than the integer `1` (`1.0`, `1.9` and `"1"` are rejected).
  It counts file-format generations, not releases; the format may still change before the agent's 1.0
  release
- `mode` missing or anything other than `local` (`fleet` is not implemented in this build)
- more than one YAML document in the file (a second `---`-separated document is silently dropped by
  a naive single-document decode, not merged or rejected)
- `disks.mode` anything other than `auto-discovery` or `selected`
- `mode: selected` with `disks.exclude` set — exclude belongs to `mode: auto-discovery` only (§4.2)
- `mode: selected` with an empty `disks.policies` map
- a device name both in `disks.exclude` and a key of `disks.policies`
- `min_iops < 1000` or `max_iops > 10000`
- `min_iops > max_iops`
- any IOPS value not a multiple of 100
- unknown keys anywhere (typos must fail loudly, not be ignored) — this is what rejects the
  superseded `device:`, `disk_id:` and `disks.selected` selectors and the removed `apiVersion`, `kind`
  and `advanced.enabled` fields
- a disk identified by a kernel device path (`/dev/vdb`, `/dev/sdb`) instead of `device_name`
- a `device_name` that cannot be the exact value VM status reports (§4.1)
- the same device name listed twice in `exclude`, or as a repeated `disks.policies` key
- `runtime.metrics.source` anything other than `guest` — `platform` is not implemented yet
- `mode: fleet` with `metrics.source: guest` (impossible — no guest access)
- `scale_down_window < scale_up_window` (a scale-down faster than scale-up invites flapping)
- `cooldown.scale_up > scale_up_window` (the agent could never act)
- `cooldown.scale_down > scale_down_window` (same reason, and it's what keeps a restarted agent's
  in-memory cooldown loss safe — design.md §9.4)
- `advanced.evaluation.scale_up_window` or `scale_down_window` ≤ 0, or a `cooldown` duration < 0
- `advanced.evaluation.collection_interval` — removed from the schema (design.md §4.2); rejected as an
  unknown key like any other superseded field
- `saturation.utilization_threshold`, `target.scale_up_utilization`, or `target.scale_down_utilization`
  outside `(0, 1]`, or non-finite (`NaN`/`Inf` — see below)
- `target.scale_up_utilization` or `target.scale_down_utilization` not strictly below
  `saturation.utilization_threshold` — a completed scale-up or scale-down leaves utilization at exactly
  its own target ratio, so either one at or above the saturation gate would immediately make that
  disk a saturated candidate again, the change having bought no real headroom
- `target.scale_down_utilization` not strictly below `target.scale_up_utilization` — this gap is a
  secondary hysteresis margin between the two target ratios themselves; closing or inverting it invites
  flapping, the same reasoning as the `scale_down_window`/`scale_up_window` rule above
- `saturation.queue_depth_threshold` ≤ 0 (strictly positive, not merely non-negative: at exactly 0 the
  queue-depth gate degenerates to always-true given a non-negative queue depth, and
  `utilization_threshold` alone would decide saturation — issue #6), or non-finite
- `saturation.latency.mode` anything other than `relative` — `absolute` is rejected too, not merely
  "not yet accepted": design.md §2/§9.4 places absolute latency thresholds out of scope for v1, and no
  Analyzer code path reads this field at all, so accepting it would silently do nothing (issue #6)
- `saturation.latency.baseline_multiplier` ≤ 1, not merely ≤ 0 — at exactly 1 the elevated-latency
  threshold (`baseline.P95 × multiplier`) equals the baseline itself, so ordinary noise around baseline
  latency would read as "elevated" continuously (issue #6), or non-finite
- `step.min_change_iops` ≤ 0 or not a multiple of 100
- `step.max_scale_up_ratio` ≤ 1 (it would never actually scale up), or non-finite
- `step.max_scale_down_ratio` outside `(0, 1)`, or non-finite
- `budget.max_changes_per_24h` ≤ 0
- any `runtime.metrics.collection_interval`, `runtime.controller.reconcile_interval`, or
  `runtime.controller.discovery_interval` ≤ 0
- `runtime.metrics.collection_interval` too long relative to the configured evaluation windows for the
  Sampler's ring buffer to ever hold enough samples to fill one — `config.ResolvedConfig.SamplerCapacity()`
  (the same formula the orchestrator uses to actually size the buffer) must be ≥ 3, the Analyzer's own
  `minSamplesForWindow` floor (design.md §4.2); below that, `WindowFullUp`/`WindowFullDown` could never become
  true no matter how long the agent ran (issue #6)
- `runtime.metrics.collection_interval` longer than any *individual* configured window — the VM
  policy's own `scale_up_window`/`scale_down_window`, or a per-disk override's — even when
  `SamplerCapacity()` above is comfortably large (pre-merge review). `SamplerCapacity()` sizes the
  buffer against only the single *longest* window across every policy; a much shorter window elsewhere
  can still be mathematically incapable of ever holding more than one real sample, since consecutive
  samples land farther apart than the window itself. `collection_interval: 20m` against the default
  `scale_up_window: 5m` is the concrete example: the buffer is plenty large (sized against the 30m
  `scale_down_window`), but `WindowFullUp` specifically could still never become true
- any of the float fields above that decode as `NaN` or `±Inf` — a bare range comparison like
  `v <= 0 || v > 1` silently lets `NaN` through (every comparison against `NaN` is `false`), so this is
  checked explicitly, separately from the range itself (issue #6)
