# ADR-003: Two write paths — disk spec for independent, VM spec for dependent

**Status:** Accepted
**Date:** 2026-08-28
**Deciders:** Product Lead (NBS)
**Supersedes:** the "independent disks only" scope in the first draft, which was wrong

## Context

A disk attached to a VM is either **dependent** (created inside the VM's spec; `metadata.usages`
contains `own`) or **independent** (a separate resource attached by `ref`; `usages` has `attach`
only).

Measured behaviour:

```
POST /compute/v1/projects/{p}/disks/{d}?updateOnly=true   {"spec":{"iops":N}}

  independent disk  → 200
  dependent disk    → 403 "Resource 'compute/.../virtualMachines/<vm>' is forbidden"
```

The 403 names the VM and reads like a permissions failure. It is not — the same credentials edit an
independent disk attached to the *same* VM successfully. It is an ownership constraint: a dependent
disk is part of the VM object, so it is edited there.

The first draft of this ADR concluded "manage independent disks only, boot disks are permanently out
of scope". That was wrong on both counts:

1. Dependent disks **can** be changed — through the VM spec rather than the disk spec:
   ```
   mws compute vm update <vm> --storage-disks 'name: <disk>
       managed: true
       disk: {spec: {diskType: <current>, iops: <new>, size: <current>}}'
   ```
2. A dependent disk **can be converted** to independent
   (`disks-spec-ref-change.html` — "вы можете сделать его независимым ресурсом, то есть «отвязать» от
   исходной ВМ"), boot disk included. The conversion is one-way: "Независимый диск нельзя сделать
   зависимым".

So this is not a scope limitation. It is two write paths for the same logical operation.

## Decision

Support both. Classify each disk at discovery and route the write accordingly:

| Class | Detection | Write path |
|---|---|---|
| Independent | `usages` has `attach` only | `UpdateDisk` — partial, `spec.iops` alone |
| Dependent | `usages` contains `own` | `vm update --storage-disks` — full spec round-trip |

## Options Considered

### Option A: Independent disks only (rejected — the first draft)

**Pros**
- One write path; every change is a single-field partial update.
- No VM-level writes, so no cross-field races.

**Cons**
- Excludes disks created the most natural way — inline in the VM spec.
- Excludes boot disks by default.
- Users would have to convert disks before getting any value, turning a one-command install into a
  migration.
- Rests on a false premise: that dependent disks cannot be changed.

### Option B: Both paths, routed by classification (chosen)

**Pros**
- Works on whatever the user already has, with no migration.
- Boot disks are manageable.
- The classification is cheap and reliable — one field already present in the discovery response.

**Cons**
- Two write paths to implement and test.
- The dependent path is read-modify-write: it must resend `diskType` and `size` alongside the new
  `iops`, so it needs a fresh read immediately before the write and carries a genuine clobber risk if
  something else edits the VM concurrently.
- The dependent path writes a VM-level object to change a disk-level property, which is a wider blast
  radius per write.

### Option C: Convert everything to independent on first sight

**Pros**
- Collapses to one write path after a one-time migration.

**Cons**
- **Irreversible.** `Независимый диск нельзя сделать зависимым` — an autoscaler must not perform
  one-way structural changes to a user's infrastructure as a side effect of being installed.
- Changes deletion semantics: a dependent disk is removed with its VM, an independent one survives.
  Silently changing that is a data-lifecycle surprise of the worst kind.

## Trade-off Analysis

Option C is disqualified on principle. An autoscaler's job is to adjust a number, not to restructure
resources irreversibly. That the conversion is one-way and alters deletion semantics makes it a
decision only the user can take. The agent may *recommend* it — the independent path is safer and
cheaper to operate — but must never do it unprompted.

Between A and B, the deciding argument is that A's premise was false. Once dependent disks are known
to be editable, excluding them means shipping something that does not work on the most common setup.

The cost of B is concentrated entirely in the dependent path, and it is real:

- **Read-modify-write race.** The VM object must be read and rewritten. A concurrent VM update
  between read and write loses one of the two changes. Mitigated by using the `etag` the API already
  returns for optimistic concurrency, and by re-reading immediately before writing.
- **Wider blast radius.** A malformed VM update could affect more than one disk's IOPS. Mitigated by
  changing exactly one field of one disk entry and asserting every other field round-trips unchanged.
- **Idempotency.** Every write sets an idempotency key, so a retry after a network failure — common
  on this network — cannot double-apply.

These are manageable with care; they are not reasons to exclude the majority of real disks.

## Consequences

**Easier**
- Works on existing infrastructure with no migration step.
- Boot disks are in scope.
- Adoption is "install and configure", not "restructure your disks, then install".

**Harder**
- Two write paths, and the dependent one is materially riskier. It needs its own tests, including a
  concurrent-modification test.
- Dependent writes must carry `etag` and re-read before writing. Without that, this is a latent
  data-loss bug rather than a theoretical one.
- The `managed: true` field appears in the docs example for `vm update --storage-disks` but is absent
  from the API response, the SDK model, and the current CLI schema. Its role needs confirming before
  relying on it.
- Classification must be re-evaluated per discovery cycle, since a disk's class can change under the
  agent's feet.

**Revisit when**
- A partial-update path for dependent disks appears — it would remove the read-modify-write risk and
  collapse the two paths.
- Operational data shows the dependent path's clobber risk is worse than modelled, at which point
  recommending conversion becomes more attractive.

## Action Items

1. [ ] `classify(disk)` from `metadata.usages`; re-evaluate every discovery cycle.
2. [ ] Independent path: `UpdateDisk` with `spec.iops` only, plus idempotency key.
3. [ ] Dependent path: read VM, mutate one disk entry, write back with `etag` and idempotency key.
4. [ ] Assert on the dependent path that every field other than the target `iops` round-trips
       unchanged; abort the write if not.
5. [ ] Add a concurrent-modification test for the dependent path specifically.
6. [ ] Confirm what `managed: true` does in `--storage-disks`, or confirm it is vestigial.
7. [ ] Surface a *recommendation* to convert dependent disks to independent, with the one-way and
       deletion-semantics caveats stated. Never convert automatically.
