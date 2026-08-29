# Platform Dependencies

A living log of MWS Cloud Platform limitations, bugs, and rough edges discovered while building the
IOPS Autoscaler — the ones that already have, or are expected to get, a feature request or bug report
filed with the platform teams.

This is **not** `docs/TODO.md`. `TODO.md` parks ideas about this project's own scope, not picked up
until a decision is made. This document is different in kind:

1. Not everything here is a blocker. The autoscaler can, and does, run with a workaround for most of
   these.
2. A workaround is not a resolution — it is a documented, temporary shape the code has today because
   the platform doesn't yet do the thing.
3. The point of writing it down is exactly so the workaround **doesn't quietly become permanent
   architecture** — something later contributors take as intentional design rather than a stopgap
   around a gap that may already be closed upstream.
4. When a platform issue is fixed, the corresponding place in the autoscaler needs a return visit:
   remove or simplify the temporary logic, don't just leave it running alongside the now-unnecessary
   workaround.
5. Taken together, this is also a log of *why* the autoscaler is shaped the way it is in the places
   where platform limitations, not product choices, drove the design.

**Resolved items are never deleted.** A resolved entry stays, with the date it was fixed and what
changed in the autoscaler afterward — that history is the record of an architectural dependency, not
clutter.

Ticket IDs, owning teams, and dates are marked `TBD` where not actually known. Nothing here is
invented to make the table look more complete than it is.

---

## Index

| Issue | Type | Status | Platform ticket | Workaround exists |
|---|---|---|---|---|
| [1. No API to list all services regardless of enabled state](#1-no-api-to-list-all-services-regardless-of-enabled-state) | Feature Request | Reported | TBD | Yes |
| [2. `monitoring.metricExporter` not included in Project Admin](#2-monitoringmetricexporter-not-included-in-project-admin) | IAM Issue | Identified | TBD | Yes |
| [3.1 No read queue depth metric (bytes)](#31-no-read-queue-depth-metric-bytes) | Metrics Issue | Identified | TBD | Yes |
| [3.2 No queue depth metrics in I/O request count](#32-no-queue-depth-metrics-in-io-request-count) | Metrics Issue | Planned | TBD | Yes |
| [3.3 Disk utilization metrics always read zero](#33-disk-utilization-metrics-always-read-zero) | Bug | Planned | TBD | Yes |
| [4. `managed` disk field not exposed via public API/CLI](#4-managed-disk-field-not-exposed-via-public-apicli) | API Limitation | Reported | TBD | Yes |
| [5. No `validate_only` on Upsert](#5-no-validate_only-on-upsert) | Feature Request | Identified | TBD | Partial |

---

## 1. No API to list all services regardless of enabled state

**Type:** Feature Request
**Status:** Reported — a feature request is already filed with the platform team; a matching endpoint
is expected.

**Problem / Current behavior**
There is an API/CLI path to list a project's **enabled** services, but no endpoint that returns the
platform's full service catalog independent of whether each one is enabled for the current project.

**Expected / Desired behavior**
An endpoint (or a parameter on the existing one) that returns every service the platform offers,
tagged with whether it's enabled for the current project — not just the enabled subset.

**Autoscaler impact**
US-016's permission preflight ("verifies it can list disks, read the VM type, and write IOPS —
failing with a message naming the missing capability rather than a raw 403") needs to tell apart two
different failures that currently look identical: a service that **exists but isn't enabled** for
this project (actionable — the operator enables it) versus a service that **doesn't exist for this
project or tier at all** (a different problem — enabling it isn't an option). CLAUDE.md already
documents that enabling a service is a separate prerequisite from granting roles, and that a project
without it returns 403 regardless of role; without a full catalog to check against, the preflight
can't distinguish which of the two is actually true.

**Current workaround**
The preflight checks only the enabled-services list. If a needed service (e.g. `compute`,
`monitoring`) is absent from it, the autoscaler reports a generic "service not enabled or unavailable"
message and asks the operator to verify manually, rather than giving a precise diagnosis.

**Platform ticket:** TBD
**Owner / Team:** TBD

**Follow-up after resolution**
Once the full-catalog endpoint exists, sharpen the preflight check to distinguish "exists but
disabled" (give exact enable instructions) from "not available for this project" (different
remediation), per US-016's own stated goal of naming the missing capability precisely.

---

## 2. `monitoring.metricExporter` not included in Project Admin

**Type:** IAM Issue
**Status:** Identified

**Problem / Current behavior**
The `monitoring.metricExporter` role — required to read Prometheus metrics from
`monitoring.mwsapis.ru` (CLAUDE.md, "Monitoring") — is not part of the platform's built-in Project
Admin role.

**Expected / Desired behavior**
A user holding Project Admin should either have every role reasonably expected to come with
administering a project, monitoring included, or the platform should make this exclusion clearly
documented so it isn't discovered by surprise.

**Autoscaler impact**
`fleet` mode always needs platform metrics, and `local` mode needs them when comparison/shadow mode
(US-005b) is enabled. An operator who is a Project Admin can reasonably assume that's sufficient and
hit an unexpected authorization failure on first run — made worse by CLAUDE.md's own measured finding
that the *wrong* credential type against this endpoint returns a misleading `503` instead of a clean
`403`.

**Current workaround**
US-016 (least-privilege guidance) documents that `local` mode needs `monitoring.metricExporter`
separately whenever comparison mode is enabled, and that `fleet` mode always needs it. The startup
permission preflight names the missing role explicitly instead of surfacing a raw 403/503.

**Platform ticket:** TBD
**Owner / Team:** TBD (IAM / project platform team)

**Follow-up after resolution**
If the role is folded into Project Admin, the separate call-out in the README/US-016 documentation
becomes unnecessary for Project Admin operators specifically — though it would still apply to any
narrower, custom role that doesn't include Project Admin's full set.

---

## 3. Platform disk metrics

One functional area (the Prometheus export of disk metrics, CLAUDE.md "Prometheus export"), several
distinct problems within it — tracked as separate sub-items since each has its own status.

### 3.1 No read queue depth metric (bytes)

**Type:** Metrics Issue
**Status:** Identified

**Problem / Current behavior**
`compute_vm_disk_write_queue_length` is exported; there is no equivalent metric for read queue depth
at all (CLAUDE.md: "write queue only — no read queue metric exists").

**Expected / Desired behavior**
A platform metric for read queue depth, in bytes, mirroring the existing write metric.

**Autoscaler impact**
In `fleet` mode, the platform metric source is the only one available (ADR-001), so the saturation
detector has no queue-based signal for read-heavy workloads there — it falls back to latency alone
(design.md §10, item 4). `local` mode is unaffected: the guest source (`/proc/diskstats`) already
reports queue depth for both read and write, in operations.

**Current workaround**
`fleet`-mode read saturation is detected from latency relative to the disk's own baseline only, with
no queue-depth confirmation for that direction.

**Platform ticket:** TBD
**Owner / Team:** TBD

**Follow-up after resolution**
Once available, fold the new read-queue signal into the `fleet`-mode saturation check on the same
footing as the existing write-queue one, narrowing the `local`/`fleet` capability gap (ADR-001).

### 3.2 No queue depth metrics in I/O request count

**Type:** Metrics Issue
**Status:** Planned — CLAUDE.md's "Platform changes committed by the NBS team" section already
records a commitment to add operations-denominated queue depth. It is not confirmed whether that
addition covers both read and write, or which.

**Problem / Current behavior**
Queue depth is only exported in bytes, which is awkward to use directly: byte-denominated queue depth
depends on block size, so it is not equivalent to a count of outstanding I/O operations and can't be
compared against operations-denominated thresholds without a conversion that itself depends on
workload-specific block size.

**Expected / Desired behavior**
Separate queue depth metrics denominated in I/O request count — read queue depth and write queue
depth as operation counts, not bytes.

**Autoscaler impact**
Would let `fleet` mode use the same operations-denominated saturation logic `local` mode already uses
via `/proc/diskstats`'s `aqu-sz`, narrowing the gap that motivated ADR-001 (design.md §10, item 3).

**Current workaround**
`fleet` mode uses the existing byte-denominated `write_queue_length`, explicitly labelled as bytes so
it is never silently compared against or mixed with the operations-denominated guest metric (US-005a).

**Platform ticket:** TBD
**Owner / Team:** TBD (NBS team, per CLAUDE.md)

**Follow-up after resolution**
Prefer the operations-denominated metric over the byte-denominated fallback once it exists and is
non-zero, per the same interface pattern US-005a already uses for utilization; retire the
byte-denominated path once it's no longer the only option, keeping it only as a further fallback.

### 3.3 Disk utilization metrics always read zero

**Type:** Bug
**Status:** Planned — CLAUDE.md's "Platform changes committed by the NBS team" section records the
NBS team's commitment to fix this.

**Problem / Current behavior**
`compute_vm_disk_ops_peak_quota_utilization` and `compute_vm_disk_bytes_peak_quota_utilization` exist
but read `0` under all measured conditions, including at full disk saturation (CLAUDE.md, "measured
against a live project": "read 0 even at full saturation — do not use"). They cannot serve as a real
autoscaling signal today.

**Expected / Desired behavior**
These metrics should reflect actual utilization relative to `spec.iops` / `spec.throughput`.

**Autoscaler impact**
Without them, the saturation gate (ADR-002 — utilization is a precondition/gate, never a trigger, but
still needed) can't be read directly off the metrics export; it has to be computed, which costs an
extra `GetDisk` call per disk just to obtain the denominator.

**Current workaround**
US-005a computes `ops_peak / spec.iops` (and the equivalent for bytes/throughput) instead of reading
the platform metric directly. A `0` reading is deliberately treated as "unavailable," not "genuinely
idle," until the bug is fixed — code carries an explicit comment that this interpretation must be
revisited once the fix ships, since a real idle disk will then legitimately report `0` too.

**Platform ticket:** TBD
**Owner / Team:** NBS team (per CLAUDE.md)

**Follow-up after resolution**
Switch to reading the platform metric directly per US-005a's existing fallback interface (prefer the
platform value when present **and non-zero**); once trustworthy, this removes the extra `GetDisk`
call if nothing else still needs `spec.iops` for that disk on the same cycle, and removes the
"treat 0 as unavailable" special case from the code.

---

## 4. `managed` disk field not exposed via public API/CLI

**Type:** API Limitation
**Status:** Reported — raised informally with the platform tech lead; no formal ticket yet.

**Problem / Current behavior**
Disks have a `managed: true/false` parameter, referenced in platform documentation, but the field
carries `x-visibility: console` in the spec — it is available to the console UI but not through the
public API or CLI.

**Expected / Desired behavior**
Expose `managed` through the public API and CLI on the same footing as the console.

**Autoscaler impact**
ADR-003 already notes this gap directly: `managed` looked like a natural, direct signal for
classifying a disk as dependent or independent (US-004a), but turned out to be unreachable
programmatically. The autoscaler instead classifies from `spec.storage.disks[].disk` having `spec`
(dependent) vs `ref` (independent), confirmed by `metadata.usages` containing `own`.

**Current workaround**
Classification never reads `managed` at all — it relies entirely on the `spec`/`ref` discriminator
plus the `usages` confirmation signal (ADR-003).

**Platform ticket:** TBD
**Owner / Team:** TBD (raised with platform tech lead, no named owner yet)

**Follow-up after resolution**
The current classification path is already complete and documented (ADR-003) — if `managed` becomes
available, it would at most serve as an additional cross-check, not a replacement. Not expected to be
a required change, just an optional simplification to reconsider.

---

## 5. No `validate_only` on Upsert

**Type:** Feature Request
**Status:** Identified

**Problem / Current behavior**
`UpdateDisk` (the independent-disk write path) supports `ValidateOnly` and works as a genuine
server-side dry run today. `UpdateVirtualMachine` (the dependent-disk write path) has no such
parameter at all — confirmed against the generated `go.mws.cloud/go-sdk@v0.19.0` client source; see
`docs/adr/ADR-007-dependent-disk-write-has-no-dry-run.md` for the detailed finding. More broadly,
there is no consistent, guaranteed `validate_only` mode across every Upsert-shaped operation.

**Expected / Desired behavior**
`Upsert(..., validate_only=true)` — runs the same validation path as a real request (bounds, the
100-IOPS step, permissions, etag/concurrency conflicts) without applying the change. Most urgently
needed on VM update, where no such path exists at all today.

**Autoscaler impact**
US-013 (dry-run mode) cannot give an equally reliable, platform-confirmed verdict for dependent and
independent disks. Before enabling live writes on a dependent disk for the first time, the autoscaler
cannot ask the platform to confirm the computed IOPS value would actually be accepted — it can only
report computed intent and rely on its own client-side checks.

**Current workaround**
Independent disks: unaffected, `ValidateOnly=true` on `UpdateDisk` already works. Dependent disks: the
dry-run report is explicitly labelled "computed, not server-checked" (ADR-007) rather than presented
as a confirmed verdict, backed only by the client-side range/step/VM-pool checks the autoscaler
already performs regardless of dry-run.

**Platform ticket:** TBD
**Owner / Team:** TBD

**Follow-up after resolution**
Once `validate_only` exists for VM update, US-013 can drop the per-class distinction — dry-run reports
become uniformly server-confirmed. `ADR-007` and the corresponding PRD acceptance criteria (US-013)
should be revisited and simplified at that point.
