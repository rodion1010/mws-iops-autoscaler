# ADR-004: Disks are addressed by `device_name` in config, never by kernel device path

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Supersedes:** the `device: /dev/vdb` and `disk_id:` selectors in the first draft of
`docs/configuration.md`, which were wrong

## Context

The operator must be able to name specific disks in config — to exclude the boot disk under
`auto-discovery`, or to attach a per-disk policy under `selected`.

The first draft of `docs/configuration.md` addressed them by kernel device name:

```yaml
disks:
  mode: selected
  selected:
    - device: /dev/vdb
    - device: /dev/vdc
```

**This is unreliable.** Kernel device names are assigned in discovery order, which is not stable
across reboots or attach/detach cycles. `/dev/vdb` today can be `/dev/vdc` tomorrow. The agent would
then apply one disk's policy to another — silently, with a config that still looks correct. On a
`cost` preset pointed at what is now the database disk, that means throttling production while the
config reads exactly as intended. There is no error to observe and nothing in the config to fix.

The same draft also used a third form in the fleet examples (`disk_id: db-data`), so the document
carried three different ways to name a disk. `docs/design.md` §4.1 and PRD US-002 had meanwhile
already settled on `deviceName` for guest-path resolution, so `configuration.md` was also the only
document out of step with the rest.

Four identifiers were available:

| Candidate | Source | Assessment |
|---|---|---|
| `deviceName` | `status.storage.disks[].deviceName` | Stable; maps to the guest path; also the metric label — config, guest and metrics all agree |
| kernel name | guest only | **Unstable.** The defect above |
| disk alias `name` | `spec.storage.disks[].name` | Stable within a VM; not the metric label, so a second mapping is needed |
| cloud disk id | `metadata.id` / `disk_name` label | Globally unique, but opaque (`iops-test-vm-mza3njax`) and unwritable by hand |

## Decision

**Disks are addressed by `device_name` in both modes**, written **without the `mws-` prefix** — the
form the operator supplied at attach time, matching `spec.storage.disks[].deviceName`.

The agent applies the prefix itself on both consuming sides:

```
config:  device_name: datadisk
guest:   /dev/disk/by-id/mws-datadisk
metric:  device_name="mws-datadisk"
API:     status.storage.disks[].deviceName == "mws-datadisk"
```

`deviceName` is unique within a VM, not globally. In `local` mode that suffices. In `fleet` mode disk
selectors are nested under their instance, so the effective key is the pair `(instance, device_name)`.

`--validate-config` rejects a kernel device path, a `device_name` carrying the `mws-` prefix, and the
superseded `device:` / `disk_id:` keys.

### Alternatives rejected

- **Kernel device path.** The defect this ADR exists to remove.
- **`device_name` in `local`, cloud disk id in `fleet`.** Two languages of addressing in one product;
  an operator moving a workload from local to fleet rewrites the whole disk block for no reason.
- **Cloud disk id everywhere.** Globally unique and mode-independent, which is genuinely better on
  the uniqueness question. Rejected because it is unreadable, cannot be written from memory, and in
  `local` mode forces an API round-trip before the agent can match a config entry to a disk it can
  already see on the filesystem.
- **A hybrid selector** accepting any of `device_name` / `disk_id` / `name`. Flexible, but it makes
  validation ambiguous and guarantees that two operators write the same config differently.
- **Prefixed form (`mws-datadisk`).** Copy-pasteable straight from `status` output and from
  `/dev/disk/by-id/`. Rejected because it does not match what the operator typed when creating the
  disk, and the prefix is a platform-side decoration rather than part of the name.

## Consequences

- One identifier spans config, guest filesystem and metric label. No translation table, no second
  mapping to keep correct.
- Policies stay attached to the disk the operator meant, across reboots and re-attachment.
- Discovery already reads `status.storage.disks[].deviceName` (design §4.1), so matching a config
  entry costs a string comparison after stripping the prefix — no extra API call.
- A `device_name` that matches no discovered disk is a config error and is reported at validation
  time, rather than silently managing nothing.
- A disk attached with no `deviceName` set falls back to `mws-{name}` (PRD US-002). Config addresses
  it by that `name`; the fallback path is unchanged by this decision.
- Existing configs using `device:` or `disk_id:` stop validating. The surface is `v1alpha1` and
  unreleased, so no migration path is provided.
