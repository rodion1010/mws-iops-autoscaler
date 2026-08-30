# MWS IOPS Autoscaler — Configuration

Derived from `docs/prd-mws-iops-autoscaler.md` (the primary document) and `docs/design.md`, with the
platform constraints from `docs/platform-reference.md` folded in. The user-facing surface described here follows the
PRD's requirements — it does not define them.

> **All numeric defaults below are design defaults, not production-validated values.** They must be
> calibrated against observe-only telemetry (design §8.5) before GA.

---

## 1. Principles

**One validated default, not a choice of knobs.** Phase 1 ships a single built-in parameter set
(§3) — there is no preset name to pick. The user-facing surface is limits plus disk selection.
Controller internals stay adjustable (`advanced`) without breaking anyone's config.

Named presets (`balanced` / `performance` / `cost`) are deferred past the MVP — see §1. This
section described three from the first draft; the `performance` one turned out to violate the
`cooldown.scale_up > scale_up_window` rule in §8 once that rule was actually implemented, and rather
than keep three tables mutually consistent under every future rule, Phase 1 ships one.

**Inheritance, not repetition.** Policy is declared once and overridden where it differs.

**v1 user-facing fields:**

```yaml
limits.min_iops
limits.max_iops
disks (selection)
```

Everything under `advanced` is experimental and API-only until telemetry justifies exposing it.

---

## 2. Policy model

```yaml
policy:
  limits:
    min_iops: 1000           # never below platform min 1000
    max_iops: 10000          # never above platform max 10000

  advanced:
    enabled: false           # opt-in; unsupported unless set

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

This is not user-facing syntax — it is never written under a `presets:` key in a config file. An
operator overrides individual fields through `policy.advanced` (opt-in, `advanced.enabled: true`),
not by naming a preset.

A `performance`-leaning set (earlier up, slower down, more headroom) and a `cost`-leaning set (later
up, tighter fit, faster down) were drafted alongside this one and are recorded in §1 for when
presets are reintroduced — post-MVP, they are not implemented, and the draft `performance` numbers
were never valid: `cooldown.scale_up: 3m` against `scale_up_window: 2m` violates §8's own rule.

---

## 4. Disk selection

### 4.1 How a disk is named

**Disks are addressed by `device_name`, in both modes.** This is the disk's `deviceName` as set when
it was attached to the VM — the same value the platform reports in
`status.storage.disks[].deviceName` and puts on the `device_name` label of every exported metric.

**Write it without the `mws-` prefix**, exactly as it was given at attach time:

```yaml
device_name: datadisk         # correct
device_name: mws-datadisk     # rejected by --validate-config
```

The platform adds the prefix on the way out — `status` returns `mws-datadisk`, the metric label reads
`device_name="mws-datadisk"`, and the guest path is `/dev/disk/by-id/mws-datadisk`. The agent applies
that prefix itself when resolving either. The config stays in the form the operator actually chose.

**Kernel device names (`/dev/vdb`, `/dev/sdb`) must never be used.** They are assigned in discovery
order, which is not stable across reboots or attach/detach cycles: `/dev/vdb` today can be `/dev/vdc`
tomorrow, and the agent would silently apply one disk's policy to another — a config that looks
correct and is wrong. See design.md §4.1.

`deviceName` is unique **within a VM**, not globally. In `local` mode that is sufficient. In `fleet`
mode disk selectors are always nested under their instance (§6.3), so the effective key is the pair
`(instance, device_name)`.

### 4.2 Strategies

Three strategies, available in both modes:

```yaml
disks:
  mode: auto-discovery          # every discovered manageable disk
```

```yaml
disks:
  mode: auto-discovery          # everything except these
  exclude:
    - bootdisk
```

```yaml
disks:
  mode: selected                # only these, each with an optional policy
  selected:
    - device_name: datadisk
    - device_name: logdisk
      policy:
        limits: {min_iops: 3000, max_iops: 10000}
```

`exclude` is a plain list of device names. `selected` entries are mappings, because each one may
carry a policy.

**`mode: selected` is exhaustive, not best-effort (issue #5).** Every `device_name` listed must
resolve against the VM's live disks on every discovery cycle — a name that doesn't match anything is a
startup/discovery error, not a logged omission. This is deliberately asymmetric with
`mode: auto-discovery`'s `exclude`, where naming a disk that doesn't exist is harmless (there's nothing
to exclude): under `selected`, the list *is* the operator's entire stated intent, so silently managing
fewer disks than named would make the agent's actual behavior diverge from what was configured, with no
signal beyond a log line. On this error the agent keeps whatever managed set the previous successful
discovery produced (never a partial one built from only the matched names) and `/readyz` reports not
ready until the mismatch is resolved — either the missing disk is attached, or the config is corrected.

Rediscovery runs on an interval, so disks attached after startup are picked up without a restart —
including recovering from this error once the VM's inventory becomes consistent with `selected` again.

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

---

## 6. Examples

### 6.1 Local, auto-discovery

```yaml
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local

policy:
  limits: {min_iops: 1000, max_iops: 10000}

disks:
  mode: auto-discovery
  exclude:
    - bootdisk

runtime:
  metrics:
    source: guest              # default in local; 'platform' is opt-in
    collection_interval: 10s
  controller:
    reconcile_interval: 30s
    discovery_interval: 5m
```

### 6.2 Local, per-disk policies

```yaml
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local

policy:
  limits: {min_iops: 1000, max_iops: 10000}

disks:
  mode: selected
  selected:
    - device_name: datadisk

    - device_name: logdisk
      policy:
        limits: {min_iops: 3000, max_iops: 10000}

    - device_name: backupdisk
      policy:
        limits: {min_iops: 1000, max_iops: 6000}
        advanced:
          enabled: true
          step: {min_change_iops: 300}   # cost-sensitive disk: fewer, larger changes

runtime:
  metrics:   {source: guest, collection_interval: 10s}
  controller: {reconcile_interval: 30s, discovery_interval: 5m}
```

### 6.3 Fleet, project → VM → disk

```yaml
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScalerFleet
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
          selected:
            - device_name: db-data
            - device_name: db-wal
              policy:
                limits: {min_iops: 5000, max_iops: 10000}

      - instance: worker-01
        disks:
          mode: auto-discovery
          exclude: [bootdisk]

      - instance: batch-01
        policy:
          limits: {max_iops: 5000}

metrics:
  source: platform             # only option in fleet
  collection_interval: 30s     # matches the platform publication interval
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

Optional user-side limits: per-disk `max_iops`, a budget ceiling, refusal to scale below the original
value, and maintenance windows.

---

## 8. Validation rules

`--validate-config` must reject, with a message naming the field:

- `apiVersion` anything other than `autoscaling.mws.cloud/v1alpha1`
- `kind` anything other than `IOPSAutoScaler` for `mode: local` (`IOPSAutoScalerFleet` is §6.3's
  fleet-mode kind, unreachable here since `mode: local` is required in this build)
- more than one YAML document in the file (a second `---`-separated document is silently dropped by
  a naive single-document decode, not merged or rejected)
- `disks.mode` anything other than `auto-discovery` or `selected`
- `mode: auto-discovery` with `disks.selected` set, or `mode: selected` with `disks.exclude` set —
  each field belongs to exactly one strategy (§4.2)
- `mode: selected` with an empty `disks.selected` list
- `min_iops < 1000` or `max_iops > 10000`
- `min_iops > max_iops`
- any IOPS value not a multiple of 100
- unknown keys anywhere (typos must fail loudly, not be ignored) — this is what rejects the
  superseded `device:` and `disk_id:` selectors
- a disk identified by a kernel device path (`/dev/vdb`, `/dev/sdb`) instead of `device_name`
- a `device_name` written with the `mws-` prefix (§4.1 — the config carries the unprefixed form)
- the same `device_name` listed twice within one VM's `selected` or `exclude` list
- `advanced` fields set while `advanced.enabled: false`
- `mode: local` with `metrics.source: platform` but no API key configured
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
