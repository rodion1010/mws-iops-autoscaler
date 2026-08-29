# MWS IOPS Autoscaler — Configuration

Derived from `docs/prd-mws-iops-autoscaler.md` (the primary document) and `docs/design.md`, with the
platform constraints from `CLAUDE.md` folded in. The user-facing surface described here follows the
PRD's requirements — it does not define them.

> **All numeric defaults below are design defaults, not production-validated values.** They must be
> calibrated against observe-only telemetry (design §8.5) before GA.

---

## 1. Principles

**One validated default, not a choice of knobs.** Phase 1 ships a single built-in parameter set
(§3) — there is no preset name to pick. The user-facing surface is limits plus disk selection.
Controller internals stay adjustable (`advanced`) without breaking anyone's config.

Named presets (`balanced` / `performance` / `cost`) are deferred past the MVP — see ADR-006. This
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
      collection_interval: 10s
      scale_up_window: 5m
      scale_down_window: 30m

    saturation:
      iops_utilization_threshold: 0.98   # GATE, not a trigger
      queue_depth_threshold: 1
      latency:
        mode: relative                   # relative | absolute (absolute unimplemented in v1)
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
| `saturation.iops_utilization_threshold` | Precondition only. A disk below it cannot be limit-starved, so signal checks are skipped. Never triggers a change on its own. |
| `saturation.queue_depth_threshold` | Units differ by source: operations in `local`, bytes in `fleet`. The agent will not compare across units. |
| `saturation.latency.baseline_multiplier` | Relative to the disk's own baseline. An absolute threshold is wrong for everyone; acceptable latency is workload-specific. |
| `target.scale_up_utilization` | Applied to estimated demand *after* saturation is confirmed: `target = demand / 0.85`. |
| `target.scale_down_utilization` | Deliberately lower, so scale-down leaves headroom instead of landing flush against current demand. |
| `step.min_change_iops` | Suppresses churn worth one or two billing steps. |
| `step.max_scale_up_ratio` | Caps a single increase. Also bounds a pathological demand estimate. |
| `budget.max_changes_per_24h` | Runaway guard. Not a cost constraint — metering is per-minute, so frequent adjustment is not inherently wasteful. |

---

## 3. Default parameter set

Phase 1's one built-in `advanced` value set — what `policy.advanced` resolves to when nothing
overrides it (ADR-006):

```yaml
evaluation:  {scale_up_window: 5m,  scale_down_window: 30m}
saturation:  {iops_utilization_threshold: 0.98, queue_depth_threshold: 1,
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
up, tighter fit, faster down) were drafted alongside this one and are recorded in ADR-006 for when
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
correct and is wrong. See ADR-004.

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

Rediscovery runs on an interval, so disks attached after startup are picked up without a restart.

**Classification is automatic and not user-configurable.** Each disk is dependent (VM-owned, written
through the VM spec) or independent (written through the disk API) — see ADR-003. Both are managed;
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
