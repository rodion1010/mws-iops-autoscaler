# ADR-001: Build as an external agent over public APIs, not a platform feature

**Status:** Accepted
**Date:** 2026-08-28
**Deciders:** Product Lead (NBS)

## Context

MWS bills NBS-PL2 IOPS as provisioned, not consumed. Users must pick a fixed limit between 1000 and
10000 and pay for it whether the workload uses it or not. Too low throttles under load; too high
burns money on idle headroom. The only remedy today is a person watching graphs.

MWS owns the whole stack, so this could be solved anywhere: inside NBS, in the hypervisor, in the
monitoring pipeline, or outside as a client of the public API. The choice determines what signals are
available, where credentials live, and what can be guaranteed.

Constraints in play:

- The exportable metric set is fixed. "Доступный для выгрузки набор метрик ограничен, самостоятельно
  расширить его невозможно." An external agent cannot add a signal it wants.
- `gpu-metrics-exporter` shows MWS already has a credential-free guest→host telemetry channel over
  vsock, but standing up a receiver requires host-side access.
- Changing a disk's IOPS is a plain synchronous `POST` returning 200. No orchestration needed.

## Decision

Build an external agent that reads the public monitoring API and calls the public Compute API. Do not
modify NBS, the hypervisor, or the monitoring pipeline.

## Options Considered

### Option A: External agent over public APIs (chosen)

| Dimension | Assessment |
|---|---|
| Complexity | Low — two HTTP clients and a decider |
| Cost | Low; no platform work, no cross-team dependency |
| Scalability | Fine; fleet mode is one process for many VMs |
| Team familiarity | High — Go, systemd, the house pattern |

**Pros**
- Ships without a platform change or another team's roadmap slot.
- Works for any customer with an API key; nothing to deploy inside NBS.
- Reversible. If it turns out badly, nothing in the platform has to be unwound.

**Cons**
- Limited to metrics the platform already exports, and one of the most useful
  (`*_quota_utilization`) is broken today.
- Detection latency is bounded by the export pipeline: 30 s publication interval, 60 s trailing
  average window, 62–90 s lag — roughly 135 s worst case.
- Credentials must live somewhere the agent can reach.
- Cannot offer a platform-level guarantee; it is best-effort automation on top.

### Option B: Feature inside NBS / the hypervisor

| Dimension | Assessment |
|---|---|
| Complexity | High — changes a billed, latency-critical data path |
| Cost | High; needs NBS roadmap capacity |
| Scalability | Best — no per-tenant deployment at all |
| Team familiarity | Low for this team; owned by NBS |

**Pros**
- Sees throttled I/O directly. The observability ceiling disappears: the hypervisor knows exactly how
  much I/O was dropped, so "how much more does this disk need" becomes answerable rather than
  inferred.
- Could act in seconds instead of minutes.
- A dead-man's switch (revert to baseline if the controller goes silent) is only trustworthy here.
- Could be sold as a product capability with an SLA.

**Cons**
- Touches the billing path. A bug provisions or bills wrongly for every tenant at once.
- Requires NBS team capacity and a much longer cycle.
- Hard to reverse once customers depend on it.

### Option C: Agent on the guest pushing over vsock to a host-side receiver

| Dimension | Assessment |
|---|---|
| Complexity | Medium-high — two components plus host deployment |
| Cost | Medium; needs host-side access |
| Scalability | Good |
| Team familiarity | High — mirrors `gpu-metrics-exporter` |

**Pros**
- No API credentials in customer guests; the blast radius of a compromised VM drops sharply.
- Guest-side collection gives sub-second granularity instead of a 60 s window.
- Reuses an existing, proven in-house pattern.

**Cons**
- Needs a receiver on the host, which is platform work — the thing Option A is chosen to avoid.
- Solves telemetry but not actuation: something still has to call the Compute API.

## Trade-off Analysis

The real axis is **signal quality versus time to ship**.

Option B is technically the right place for this. Throttling is only fully visible where throttling
happens, and only the platform can promise a floor. But it costs an NBS roadmap slot and puts a new
control loop on the billing path.

Option A accepts worse signals in exchange for shipping now and being reversible. The measurements
say those signals are good enough: latency histograms and queue depth are exported, and at steady
state `read_ops_peak` lands within 0.2% of the true limit. The gap is transition speed — roughly two
minutes of blindness after a load change — which is tolerable when the scale-up dwell is five minutes
anyway.

Option C is the security-optimal shape but requires the same host-side work as B without B's payoff.

The deciding argument for A is optionality: it produces a working decision algorithm, tuned against
real workloads, that can later be lifted into the platform. Starting with B means designing the
algorithm before knowing whether it works.

## Consequences

**Easier**
- No cross-team dependency; ships on this team's own schedule.
- Failure is contained — the worst case is a wrong `iops` value on one disk, bounded by FR-12.
- The decision algorithm gets validated against real load before anyone commits platform effort.

**Harder**
- The agent must reconstruct utilization as `ops_peak / spec.iops` because `*_quota_utilization`
  returns 0. Requires a second API call purely to get a denominator.
- The VM ceiling (`3000 × vCPU`) is not enforced by the platform, so the agent is the only thing
  preventing billed-but-unusable IOPS.
- No dead-man's switch. If the agent dies after scaling up, the disk stays expensive until a human
  notices. A platform-side revert is impossible from here.
- Dependent (VM-owned) disks cannot be modified through the disk API at all, so the boot disk is
  permanently out of scope — see ADR-003.

**Revisit when**
- `*_quota_utilization` and operations-denominated queue depth ship, as the NBS team has committed.
  That removes the worst signal gaps and may change the calculus.
- Adoption justifies a platform feature, at which point this agent becomes the reference
  implementation rather than the product.

## Action Items

1. [ ] Build the Compute API client on the SDK's `DiskSugared.UpdateDisk` (US-011).
2. [ ] Build the monitoring client against `monitoring.mwsapis.ru` (US-005).
3. [ ] Enforce `[1000, min(10000, 3000 × vcpuCount)]` in the agent (FR-12), since the platform does
       not.
4. [ ] Document the absent dead-man's switch as a known limitation for operators.
5. [ ] Re-evaluate this ADR when the committed metric fixes land.
