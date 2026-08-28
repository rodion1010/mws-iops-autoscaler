# MWS IOPS Autoscaler — Configuration

Companion to `docs/design.md`. Derived from the product-side design draft, with the platform
constraints from `CLAUDE.md` folded in.

> **All numeric defaults below are design defaults, not production-validated values.** They must be
> calibrated against observe-only telemetry (design §8.5) before GA.

---

## 1. Principles

**Presets, not knobs.** The user-facing surface is a preset name plus limits plus disk selection.
Controller internals stay adjustable without breaking anyone's config.

**Inheritance, not repetition.** Policy is declared once and overridden where it differs.

**v1 user-facing fields:**

```yaml
preset
limits.min_iops
limits.max_iops
disks (selection)
```

Everything under `advanced` is experimental and API-only until telemetry justifies exposing it.

---

## 2. Policy model

```yaml
policy:
  preset: balanced           # balanced | performance | cost

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

## 3. Presets

```yaml
presets:
  balanced:                              # platform default
    evaluation:  {scale_up_window: 5m,  scale_down_window: 30m}
    saturation:  {iops_utilization_threshold: 0.98, queue_depth_threshold: 1,
                  latency: {mode: relative, baseline_multiplier: 1.5}}
    target:      {scale_up_utilization: 0.85, scale_down_utilization: 0.70}
    cooldown:    {scale_up: 5m,  scale_down: 30m}
    step:        {min_change_iops: 200, max_scale_up_ratio: 2.0, max_scale_down_ratio: 0.5}

  performance:                           # earlier up, slower down, more headroom
    evaluation:  {scale_up_window: 2m,  scale_down_window: 60m}
    saturation:  {iops_utilization_threshold: 0.95, queue_depth_threshold: 1,
                  latency: {mode: relative, baseline_multiplier: 1.25}}
    target:      {scale_up_utilization: 0.75, scale_down_utilization: 0.60}
    cooldown:    {scale_up: 3m,  scale_down: 60m}
    step:        {min_change_iops: 200, max_scale_up_ratio: 2.0, max_scale_down_ratio: 0.75}

  cost:                                  # later up, tighter fit, faster down
    evaluation:  {scale_up_window: 10m, scale_down_window: 20m}
    saturation:  {iops_utilization_threshold: 1.00, queue_depth_threshold: 2,
                  latency: {mode: relative, baseline_multiplier: 2.0}}
    target:      {scale_up_utilization: 0.90, scale_down_utilization: 0.80}
    cooldown:    {scale_up: 10m, scale_down: 20m}
    step:        {min_change_iops: 300, max_scale_up_ratio: 1.5, max_scale_down_ratio: 0.5}
```

Note `cost` uses a `scale_down_window` (20m) shorter than `balanced` (30m) — it reclaims idle capacity
sooner, accepting more risk of scaling down before a lull ends.

---

## 4. Disk selection

Three strategies, available in both modes:

```yaml
disks:
  mode: auto-discovery          # every discovered manageable disk
```

```yaml
disks:
  mode: auto-discovery          # everything except these
  exclude:
    - /dev/vda
```

```yaml
disks:
  mode: selected                # only these, each with an optional policy
  selected:
    - device: /dev/vdb
    - device: /dev/vdc
      policy:
        preset: performance
```

Disks are identified by guest device path in `local` mode and by cloud disk id in `fleet` mode.
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
  preset: balanced
  limits: {min_iops: 1000, max_iops: 10000}

disks:
  mode: auto-discovery
  exclude:
    - /dev/vda

runtime:
  metrics:
    source: guest              # default in local; 'platform' is opt-in
    collection_interval: 10s
  controller:
    reconcile_interval: 30s
```

### 6.2 Local, per-disk policies

```yaml
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local

policy:
  preset: balanced
  limits: {min_iops: 1000, max_iops: 10000}

disks:
  mode: selected
  selected:
    - device: /dev/vdb

    - device: /dev/vdc
      policy:
        preset: performance
        limits: {min_iops: 3000, max_iops: 10000}

    - device: /dev/vdd
      policy:
        preset: cost
        limits: {min_iops: 1000, max_iops: 6000}

runtime:
  metrics:   {source: guest, collection_interval: 10s}
  controller: {reconcile_interval: 30s}
```

### 6.3 Fleet, project → VM → disk

```yaml
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScalerFleet
mode: fleet

project:
  policy:
    preset: balanced
    limits: {min_iops: 1000, max_iops: 10000}

  instances:
    discovery:
      mode: all

    overrides:
      - instance: database-01
        policy: {preset: performance}
        disks:
          mode: selected
          selected:
            - disk_id: db-data
            - disk_id: db-wal
              policy:
                preset: performance
                limits: {min_iops: 5000, max_iops: 10000}

      - instance: worker-01
        disks:
          mode: auto-discovery
          exclude: [{disk_id: boot}]

      - instance: batch-01
        policy:
          preset: cost
          limits: {max_iops: 5000}

metrics:
  source: platform             # only option in fleet
  collection_interval: 30s     # matches the platform publication interval
controller:
  reconcile_interval: 30s
```

---

## 7. Guardrails the user cannot override

Regardless of preset or override:

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
- unknown preset name
- unknown keys anywhere (typos must fail loudly, not be ignored)
- `advanced` fields set while `advanced.enabled: false`
- `mode: local` with `metrics.source: platform` but no API key configured
- `mode: fleet` with `metrics.source: guest` (impossible — no guest access)
- `scale_down_window < scale_up_window` (a scale-down faster than scale-up invites flapping)
- `cooldown.scale_up > scale_up_window` (the agent could never act)
