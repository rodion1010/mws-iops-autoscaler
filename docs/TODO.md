# TODO — parking lot

Ideas and problems noticed while working on something else. This file exists so a good thought is
not lost and does not derail whatever is currently in progress.

## How to use this file

**Do not pick work from here.** Items are recorded, not scheduled. An item moves out of this file
only when we explicitly decide to work on it — not because it looks small, not because it is adjacent
to the current task, not because the file was open.

Adding to it is always fine. Acting on it requires a decision.

When an item is picked up: state which one, do the work, delete the entry in the same change.

Format: `### <short title>` + what the problem is, why it matters, and any options already considered.
Leave the decision open unless it is genuinely settled.

---

## Open items

### Disk identification in config must not use kernel device names

Current config examples in `docs/configuration.md` address disks by kernel name:

```yaml
disks:
  mode: selected
  selected:
    - device: /dev/vdb
    - device: /dev/vdc
```

**This is unreliable.** Kernel device names are assigned in discovery order, which is not stable
across reboots or attach/detach cycles. `/dev/vdb` today can be `/dev/vdc` tomorrow, and the
autoscaler would silently apply one disk's policy to another — a config that looks correct and is
wrong.

It also contradicts what discovery already established (`CLAUDE.md`, guest device mapping): the
stable identifier is `deviceName`, exposed at a deterministic path:

```
/dev/disk/by-id/mws-{deviceName}          # deviceName set at attach time
/dev/disk/by-id/mws-{name}                # when deviceName was not set
/dev/disk/by-id/scsi-0MWS_PersistentDisk_mws-{deviceName}   # second, undocumented alias
```

Candidate identifiers, to choose between later:

| Candidate | Source | Notes |
|---|---|---|
| `deviceName` | `status.storage.disks[].deviceName` (returned with the `mws-` prefix) | Stable; maps directly to the guest path; also the label used in the exported metrics, so config, guest and metrics all agree |
| `name` | the disk's alias within the VM (`spec.storage.disks[].name`) | Stable within a VM; not globally unique |
| cloud disk id | `metadata.id` / `disk_name` in metrics | Globally unique; opaque and unfriendly to type |

`deviceName` looks strongest because it is the one identifier that is simultaneously in the config,
on the guest filesystem, and on the metric label — but this is not decided.

Affects: `docs/configuration.md` (all examples and the disk-selection section), `docs/design.md`
§4.1 and §5.2, PRD US-002 and US-004.
