# ADR-002: Platform metrics as the primary source, guest collection as a mode-specific optimisation

**Status:** Accepted
**Date:** 2026-08-28
**Deciders:** Product Lead (NBS)

## Context

The decider needs to answer one question per disk: is it saturated, and by how much. That question is
harder than it looks, because **when a disk hits its limit the IOPS counter pins to the limit and
stops carrying information**. A disk reading 1000/1000 might need 1100 or 10000; the counter cannot
tell them apart.

Two collection paths exist, and the agent runs in two modes (`local` on the VM, `fleet` centrally),
so the choice is not automatically the same in both.

What the measurements established (see `CLAUDE.md`):

- The platform **does** export saturation signals: read/write latency histograms and a write queue
  depth. The initial assumption that guest collection would be mandatory was wrong.
- Platform op counters match the guest exactly — 960744 reads in `/proc/diskstats` versus 960770 in
  `read_latency_hist_count`. The two views are interchangeable for counting.
- Platform timing: 30 s publication, 60 s trailing average, 62–90 s lag. Worst-case detection ≈ 135 s.
- `*_quota_utilization` — the metrics purpose-built for this — return 0 at every load level.
- There is no read-queue metric, and the write queue is byte-denominated despite the docs.

## Decision

Use the platform monitoring API as the primary and default metric source in both modes. In `local`
mode, allow `/proc/diskstats` as an opt-in optimisation. Resolve utilization through an interface
that prefers `*_quota_utilization` when it is non-zero and falls back to `ops_peak / spec.iops`.

## Options Considered

### Option A: Platform metrics only (chosen as default)

| Dimension | Assessment |
|---|---|
| Complexity | Low — one HTTP client, one parser |
| Cost | Low; nothing deployed to customer VMs |
| Scalability | Excellent — one call covers a whole project |
| Team familiarity | High |

**Pros**
- No agent inside customer guests: no packaging, no upgrades, no credentials in the VM.
- Identical code path in both modes; one thing to test.
- Fleet mode works without any guest access at all.

**Cons**
- 135 s worst-case detection latency.
- Utilization must be computed, costing an extra call for `spec.iops`.
- No read-queue signal; read-heavy workloads rely on latency alone.

### Option B: Guest `/proc/diskstats` only

| Dimension | Assessment |
|---|---|
| Complexity | Medium — plus deployment and device mapping |
| Cost | Medium; an agent on every managed VM |
| Scalability | Poor for fleets — N deployments |
| Team familiarity | High |

**Pros**
- Sub-second granularity; no publication lag.
- Read **and** write queue depth, both in operations — `aqu-sz` directly.
- No monitoring API quota or availability dependency.

**Cons**
- Impossible in `fleet` mode without agents everywhere, which is the deployment cost the product is
  trying to avoid.
- Sees the guest's view only. It cannot see the VM-wide pool, so the ADR-001 saturation guard would
  have to call the API anyway.
- Requires the cloud-disk→device mapping, which works but is one more thing to get wrong.

### Option C: Hybrid — guest for saturation, platform for limits (chosen for `local` only)

| Dimension | Assessment |
|---|---|
| Complexity | High — two collectors, two clock domains |
| Cost | Medium |
| Scalability | Medium |
| Team familiarity | High |

**Pros**
- Best signal quality available without platform changes.
- Detects saturation in seconds while still reconciling against real limits.

**Cons**
- Two sampling cadences to reconcile: 1 s local against a 60 s trailing platform window. Comparing
  them naively produces contradictions.
- Doubles the surface where a metric bug can hide.

## Trade-off Analysis

The decisive finding is that **the platform already exports saturation signals**. That removes the
argument that made guest collection mandatory and turns it into an optimisation.

Given that, Option A wins on the property that matters most for a product: it works identically in
both modes with one code path, and requires nothing installed on customer machines. The 135 s latency
is real but sits comfortably inside a 5-minute scale-up dwell, which is itself floored by the same
pipeline.

Option B fails on `fleet` mode, which is the mode with the broader audience.

Option C is worth having in `local` mode, where the agent is on the box anyway and local reads cost
nothing — but as an opt-in, not the default. It buys latency, not correctness, and the two clock
domains are a genuine source of bugs. Making it default would pay full complexity for a benefit only
some users need.

**On `*_quota_utilization`:** the natural design reads it directly. It returns 0 always, so the agent
must compute utilization instead. The NBS team has committed to fixing it, so this is temporary — but
the fallback has a trap. Today `0` means "broken, ignore". After the fix, `0` will legitimately mean
"idle disk". The heuristic must change when the fix ships or the agent will treat idle disks as
having no data.

## Consequences

**Easier**
- Fleet mode needs no guest footprint whatsoever.
- One metric parser to write and test.
- Adopting the platform's fixes is a config change, not a rewrite (FR-23).

**Harder**
- An extra `GetDisk` call per disk purely to obtain `spec.iops` as a denominator.
- Read-heavy saturation rests on latency histograms alone, with no queue signal to corroborate.
- The queue metric is byte-denominated, so it cannot be compared to guest `aqu-sz` without
  conversion. Mixing the two silently would be a real bug — hence the explicit unit labelling in
  US-005a.
- The `quota_utilization == 0` fallback is a landmine with a scheduled detonation date.

**Revisit when**
- `*_quota_utilization` starts returning real values — flip the preference and change the zero
  handling.
- An operations-denominated queue metric appears — prefer it and demote the byte one.
- A read-queue metric appears — strengthen the read-side saturation test.

## Action Items

1. [ ] Implement `UtilizationSource` with platform-first, computed-fallback resolution (FR-23).
2. [ ] Log and export which source produced each decision (FR-24).
3. [ ] Comment the `== 0` check with a pointer to this ADR and the expected fix.
4. [ ] Make guest collection opt-in in `local` mode only; default off.
5. [ ] Set the minimum window length from the measured 30 s publication interval, not a guess.
