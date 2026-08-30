# ADR-001: Metric source is chosen by mode, not globally

**Status:** Accepted
**Date:** 2026-08-28
**Deciders:** Product Lead (NBS)

## Context

The agent runs in two modes with fundamentally different access:

- **`local`** — inside the VM whose disks it manages. The guest filesystem is right there.
- **`fleet`** — outside, managing many VMs. There is no guest access and nobody is going to grant it.

Two metric sources exist, measured (`docs/platform-reference.md`):

| | Guest (`/proc/diskstats`, `iostat`) | Platform (`monitoring.mwsapis.ru`) |
|---|---|---|
| Granularity | 1 s or finer | 30 s publication, 60 s trailing window |
| Lag | none | 62–90 s |
| Queue depth | read **and** write, in operations (`aqu-sz`) | write only, byte-denominated |
| Latency | per-device, immediate | histogram, ns buckets |
| Credentials | none — reads a local file | API key + `monitoring.metricExporter` role |
| Network | none | HTTPS over the public internet |
| VM-wide pool view | no | yes |

The counters agree: 960744 reads in `/proc/diskstats` versus 960770 in
`compute_vm_disk_read_latency_hist_count`. The two views measure the same thing.

The network path matters here. Every MWS API call in this project has been intermittently failing —
`SSL_ERROR_SYSCALL`, endpoint discovery taking 15 s against a 5 s default timeout. Making a
per-10-second control loop depend on that link, when the data is available locally as a file read, is
building fragility for no gain.

## Decision

**`local` mode: guest metrics are the primary source.** The monitoring API is an option, used only
for VM-level pool data that the guest cannot see.

**`fleet` mode: platform metrics only.** No guest access is assumed or required.

## Options Considered

### Option A: Platform metrics everywhere, one code path

| Dimension | Assessment |
|---|---|
| Complexity | Low — one collector |
| Cost | Requires API key + role on every managed project |
| Scalability | Excellent |
| Team familiarity | High |

**Pros**
- Single implementation to write and test.
- Identical behaviour in both modes.

**Cons**
- In `local` mode it demands an API key and the `monitoring.metricExporter` role for data the agent
  could read from a local file with no credentials at all.
- Routes a control loop through an unstable internet path.
- Throws away the guest's better signals: read queue depth, operations-denominated queue,
  sub-second granularity.
- 135 s worst-case detection latency where the guest offers ~1 s.

### Option B: Source chosen by mode (chosen)

| Dimension | Assessment |
|---|---|
| Complexity | Medium — two collectors behind one interface |
| Cost | `local` needs no metrics credentials at all |
| Scalability | Excellent in both modes |
| Team familiarity | High |

**Pros**
- `local` needs no monitoring API key and no metrics role — a materially smaller permission ask.
- `local` decisions are based on data ~2 minutes fresher.
- `local` gets read queue depth, which the platform does not expose at all.
- `fleet` needs nothing installed on customer VMs.

**Cons**
- Two collectors to build and keep behaving consistently.
- The two modes will not always reach identical decisions on identical load, because their inputs
  differ in granularity. That has to be expected rather than treated as a bug.

### Option C: Guest metrics everywhere

Rejected outright: `fleet` mode would require an agent on every managed VM, which is precisely the
deployment burden `fleet` exists to avoid.

## Trade-off Analysis

The instinct is to unify on one source for a single code path. That is the wrong trade here, because
the two modes are not variations of one deployment — they are different products with different
access.

For `local`, the platform path is strictly worse on every axis that matters: it needs credentials the
guest path does not, it crosses an unreliable network, it is two minutes staler, and it carries fewer
signals. There is no dimension on which it wins. Choosing it for code-path symmetry would be paying a
real cost for a cosmetic benefit.

For `fleet`, the guest path is simply unavailable.

The cost of Option B is genuine — two collectors, and a class of "why did local decide differently
from fleet" questions. That is handled by putting both behind one `MetricSource` interface returning
the same shape, and by recording which source produced each decision.

One asymmetry remains: the guest cannot see the VM-wide IOPS pool, which the saturation guard needs
(ADR-002). So `local` mode still makes occasional API calls — but on the slow path (discovery,
VM type lookup), not in the per-cycle control loop.

## Consequences

**Easier**
- `local` mode installs with only the Compute API credential needed to *write* IOPS. No metrics key,
  no `monitoring.metricExporter` role — a much easier security conversation with a customer.
- `local` reacts in seconds rather than minutes.
- Read-heavy workloads are properly served in `local` mode, where a read queue signal exists.

**Harder**
- Two collectors, two sets of tests, two failure modes.
- Metric semantics differ: guest queue depth is in operations, platform is in bytes. Mixing them
  silently would be a real bug — units must be explicit at the interface boundary.
- Latency baselines are not portable between modes; each source needs its own baseline.
- `local` still needs the Compute API for VM pool data, so it is not fully offline.

**Revisit when**
- The platform ships operations-denominated queue depth and read queue metrics. That narrows the gap
  and may make platform-only viable for `local` too.
- Someone actually asks for guest agents in `fleet` mode — currently assumed to be nobody.

## Action Items

1. [ ] Define `MetricSource` with one shape and explicit units on every field.
2. [ ] Implement `GuestSource` (`/proc/diskstats`) and `PlatformSource` (Prometheus export).
3. [ ] Default `local` → guest, `fleet` → platform; allow `local` to opt into platform.
4. [ ] Keep VM pool lookups on the slow path in both modes, out of the per-cycle loop.
5. [ ] Record the active source per decision in logs and exported metrics.
