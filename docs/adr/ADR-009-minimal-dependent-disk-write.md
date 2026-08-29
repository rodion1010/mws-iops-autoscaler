# ADR-009: Dependent-disk writes send a minimal partial body, never a full-spec resend

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Amends:** ADR-003's dependent write path (its class-routing decision and boot-disk scope are
unchanged); supersedes ADR-003's "full spec round-trip" / "assert every other field byte-identical"
description and Action Item 4

## Context

ADR-003 chose to route dependent-disk writes through the VM update endpoint (`vm update
--storage-disks`), since the disk API returns 403 on a VM-owned disk. It described the write as a
**full-spec round trip**: read the VM, "mutate one disk entry", "assert every other field round-trips
unchanged", and its Action Item 4 called for exactly that assertion. `docs/design.md` §4.7 mirrored
this: "assert every other field byte-identical to what was read."

Implementing that literally means reconstructing every untouched disk entry's `boot`, `deviceName`,
`diskType`, and `size` from the GET response and resending them as part of the write — because
`storage.disks[]` is a collection, and MWS's Upsert semantics treat an existing element **omitted**
from the request as deleted, so every alias has to appear in the body regardless.

That reconstruction is where the risk actually lives. CLAUDE.md's "DiskSpecRequest mutability"
findings are explicit: `name`, `boot`, `deviceName`, and `diskType` are immutable on update, and the
API accepts them **omitted entirely** — there is no requirement to resend the current value, only a
rule that if you do, it must match. A value round-tripped through a response→domain→request
translation is exactly the kind of thing that can silently drift: a `bytesize.ByteSize` reparsed and
re-serialized, an enum re-stringified, a status-derived field written back into a spec-shaped request.
Getting it wrong doesn't fail loudly — depending on the field, the platform either rejects the write
(safe but confusing) or accepts a value nobody intended to change (unsafe). Building a full-spec body
purely to satisfy a self-imposed "byte-identical" assertion adds this risk to reduce nothing: the
assertion is a client-side rule invented to compensate for round-tripping data that never needed to be
sent as a value at all.

## Decision

**Dependent-disk writes send every current alias, in order, carrying nothing but its `name` — except
the target alias, which additionally carries `disk.spec.iops`.** No entry, target included, carries
`boot`, `deviceName`, `diskType`, `size`, or `ref`.

```json
{
  "metadata": { "etag": "<etag>" },
  "spec": {
    "storage": {
      "disks": [
        { "name": "bootdisk", "disk": { "spec": { "iops": 5000 } } },
        { "name": "datadisk" }
      ]
    }
  }
}
```

Every alias must still be present — that part of ADR-003 is unchanged, and is not what this ADR
revises. What changes is what each entry carries: the collection key alone for untouched disks,
nothing reconstructed and resent as an assertion of no-change. The etag is still mandatory (US-011a)
and is checked before any network call; an empty etag is refused, not defaulted.

This is a purely internal request-shaping decision — it does not change ADR-003's classification
logic, the 403-on-dependent-disk discriminator, or the recommendation to convert dependent disks to
independent where the user wants that.

## Options Considered

### Option A: Full-spec resend with a byte-identical assertion (ADR-003's original description, rejected)

**Pros**
- Reads as an explicit, checkable safety net: "assert nothing else changed."

**Cons**
- Requires reconstructing immutable fields from a lossy response→domain→request translation, which is
  itself a source of the exact class of bug it's meant to guard against.
- The assertion only catches a mismatch the *client* introduces; it does nothing about the platform
  silently accepting a resent value as an intended change.
- More code, more fields to keep in sync with the SDK's model across versions, for a guarantee the
  platform's own immutability rules already provide for free when a field is omitted.

### Option B: Minimal partial body — name-only entries, target carries only the changed field (chosen)

**Pros**
- Nothing is sent that isn't either the collection key (structurally required) or the actual change.
  There is no value to get wrong, because there is no value to resend.
- Matches how the independent-disk path already works (`{"spec":{"iops":N}}`, nothing else) —
  one mental model for "partial update" across both write paths instead of two.
- Removes an entire class of translation bugs (this ADR exists because one was found and fixed —
  see GitHub issue #3).

**Cons**
- Relies on the platform's documented immutability behavior (fields may be omitted, not just
  resent-unchanged) holding as described in CLAUDE.md rather than the client enforcing it directly.
  Mitigated: this is exactly what CLAUDE.md's "DiskSpecRequest mutability" section verified against
  the OpenAPI spec's own immutability notes, not an assumption.

### Option C: Convert every dependent disk to independent to avoid the VM write path entirely

Out of scope here — this is ADR-003's Option C, already rejected there as an irreversible structural
change the agent must never perform unprompted. Unaffected by this ADR.

## Consequences

**Easier**
- One write-path mental model (`{changed field(s) only}`, collection keys where structurally
  required) instead of two.
- Removes the `bytesize`/`computeref` reconstruction code and its imports from `sdk_client.go` — less
  surface, less to keep aligned with SDK model changes.
- The wire shape is directly testable by marshaling `buildDependentUpdateBody`'s output and asserting
  on it (`internal/compute/sdk_client_test.go`), rather than only reviewable by eye.

**Harder**
- None identified beyond Option B's already-mitigated con above.

**Revisit when**
- The platform's immutable-field-omission behavior changes (i.e., CLAUDE.md's verified findings no
  longer hold) — re-verify with `validateOnly`-style testing before trusting omission again.

## Action Items

1. [x] `DependentUpdateRequest` carries `Aliases []string` and `TargetAlias`/`TargetIOPS`, not a
       reconstructed `[]StorageDisk`.
2. [x] `buildDependentUpdateBody` builds name-only entries for every alias except the target.
3. [x] Empty etag refused before any network call (`sdk_client.go`, `write.go`).
4. [x] Regression test on the marshaled JSON shape, asserting immutable fields never appear
       (`internal/compute/sdk_client_test.go`).
5. [ ] Supersedes ADR-003's Action Item 4 ("assert every field other than target iops round-trips
       unchanged") — that assertion is no longer meaningful once those fields are never sent; ADR-003
       is not rewritten (see the sync rule), this item records the supersession.
