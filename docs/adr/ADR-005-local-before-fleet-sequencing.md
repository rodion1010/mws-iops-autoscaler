# ADR-005: Local mode ships complete before fleet mode is built; platform metrics validated via shadow mode

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Supersedes:** the release-phase order in the first draft of PRD §6, which put fleet mode ahead of
bidirectional scaling and gated all actuation behind a separate observe-only release; also amends
ADR-002's "Rollout" section, which staged observe-only as a release ahead of scale-up-only rather
than as a validation step within it

## Context

The first draft of the release phasing (PRD §6, first version) ordered work as:

1. Observe & Recommend — `local` mode, dry-run only, nothing writes
2. Actuate — `local` mode, scale-up only
3. Fleet mode (noted as buildable in parallel with 2, since it only depends on phase 1)
4. Bidirectional — scale-down added
5. Distribution — packaging, including the container image

This followed `docs/design.md` §9.2's existing argument almost directly: ship `local` first because
it is cheaper to validate against, and never write anything until the estimator has been observed
being right. It put `fleet` mode ahead of scale-down because nothing in the dependency graph forced
otherwise.

Product review raised three objections:

1. **Platform metrics are not fleet-exclusive.** ADR-001 already decided `local` mode may opt into
   the platform source (Action Item 3: "allow `local` to opt into platform"); tying its build to
   `fleet` mode was an accident of the phase draft, not a real dependency. Building it earlier, and
   validating it against real load in `local` mode — where a trusted guest reading exists to check it
   against — is strictly safer than `fleet` mode being the first place it ever sees production
   traffic, with no fallback if it disagrees.
2. **A local-only agent that can only scale up is not the most valuable thing to ship second.** From
   an operator's standpoint, a complete single-VM agent (both directions) is more useful than a
   two-mode agent where neither mode is complete. Fleet mode adds reach, not capability; it should
   extend a finished capability, not race it.
3. **Container distribution belongs with fleet, not general packaging.** A container is the natural
   shape for a central controller; a systemd unit / `.deb` is the natural shape for something
   installed on the VM it manages. The original US-015 bundled both under one "Packaging" story with
   no phase distinction.

## Decision

**`local` mode ships complete, including bidirectional scaling, before `fleet` mode is built at
all.** Revised order (PRD §6):

1. **MVP** — `local` mode, scale-up, binary/systemd/`.deb`. Actuation is *in* the MVP, not gated
   behind it.
2. **Platform metrics, validated in `local`** — the platform `MetricSource`, plus a new comparison
   (shadow) mode: guest and platform run concurrently, only guest ever actuates, both decisions are
   journaled and their divergence surfaced (new story, US-005b).
3. **Bidirectional** — scale-down, in `local` mode, calibrated from phases 1–2 telemetry.
4. **Fleet mode** — discovery across VMs, config inheritance, container image. Reuses the
   `PlatformSource` from phase 2 and the `Decider` from phase 3 unchanged.
5. **Further fleet capabilities** — left open, not yet broken into stories.

**Scale-up actuation moves into the MVP.** The original "observe before write" principle is kept, but
as an operator workflow (`--dry-run`, validated against the live test environment) rather than a
release gate — justified because scale-up's blast radius is bounded: it can only add capacity, and
any overspend is visible and reversible on the very next cycle. Scale-down does not get this
treatment; a wrong scale-down throttles the workload, which is a real failure, not just a wasteful
one, so it keeps its own phase after scale-up has run live.

## Options Considered

### Option A: Original order (observe-only → actuate → fleet → bidirectional → packaging)

Matches `docs/design.md` §9.2 as first written. Rejected: delays any real actuation to phase 2 and
bidirectional to phase 4, by which point fleet mode — built in between — has been running on an
unvalidated platform metric source with no local shadow-run history to have caught a bad decision
cheaply first.

### Option B: Chosen order — local complete, then fleet

Ships a fully useful (if narrower-reach) agent as the MVP; validates the riskier metric source (M7)
before the mode that has no fallback for it (M10) depends on it exclusively; scale-down waits for
real telemetry instead of guessing thresholds cold.

**Cons, accepted:** the MVP no longer has a distinct, separately-releasable "zero risk" milestone —
the safety argument rests on scale-up's bounded blast radius rather than on a phase boundary. If that
assumption breaks (see Consequences), it needs revisiting.

### Option C: Build fleet and local in lockstep from the start

Rejected: doubles the surface being stabilized at once, and contradicts ADR-001's own reasoning that
the cheaper, single-VM environment should absorb first-contact bugs before the harder-to-debug
multi-VM one does.

## Consequences

**Easier**
- MVP delivers a genuinely useful agent immediately — real writes, not just recommendations.
- Platform metrics get exercised against real load, with a trusted fallback alongside them, well
  before `fleet` mode is built on top of them with nothing to fall back to.
- `fleet` mode, once built, only adds discovery scope, inheritance, and packaging — the decision
  logic and the metric source it depends on are both already proven.

**Harder**
- No separately-releasable zero-actuation milestone. If scale-up ever turns out to have a real
  failure mode beyond overspend (for example, budget exhaustion starving something else
  billing-relevant), this bet needs revisiting — the mitigating argument here is specific to scale-up
  and does not carry over automatically.
- Comparison mode (US-005b) is new scope not in the first draft: two `MetricSource` implementations
  running concurrently per disk, in `local` mode, with independent window/baseline state per ADR-001.

**Revisit when**
- M8 (comparison mode) shows platform metrics diverging from guest by more than noise on real
  workloads. If the divergence rate is high, `fleet` mode (M10) should not be released depending on
  the platform source exclusively until that is understood — this is exactly the dependency the
  shadow-run phase exists to de-risk.
