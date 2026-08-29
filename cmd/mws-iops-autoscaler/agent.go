package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rodion1010/mws-iops-autoscaler/internal/actuator"
	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/health"
	"github.com/rodion1010/mws-iops-autoscaler/internal/journal"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
	"github.com/rodion1010/mws-iops-autoscaler/internal/registry"
	"github.com/rodion1010/mws-iops-autoscaler/internal/state"
	"github.com/rodion1010/mws-iops-autoscaler/internal/telemetry"
)

// Agent wires design.md §3.1's whole pipeline together: Discovery/Registry
// (slow path, DiscoveryInterval) → Sampler (fast path, CollectionInterval)
// → Analyzer → Decider → Actuator (reconcile, ReconcileInterval), with
// State/Journal/Observability on the side. It is the orchestrator M0-M5
// deliberately left unbuilt — every package above is pure and testable on
// its own; this is where their I/O ends meet.
type Agent struct {
	cfg      *config.Config
	resolved *config.ResolvedConfig
	vmName   string
	dryRun   bool

	// allowScaleDown is defense in depth for issue #14's Phase 1 gate:
	// PolicyDecider already refuses to ever return ActionScaleDown when
	// its own allowScaleDown is false, so this check should never fire in
	// practice — but Decider is an interface, and Agent must not simply
	// trust whatever implementation it was handed to have gotten the gate
	// right. Set from the same single constructor argument as the
	// Decider's own (main.go) — one capability, not two independently
	// maintained copies of it.
	allowScaleDown bool

	// maxDiscoveryAge bounds how old the last successful discover() may be
	// before reconcile refuses to act on it (issue #15's "fail closed on
	// stale... discovery snapshots"). Unlike sample staleness, a failed
	// discover() call is immediately visible (readiness.SetDiscoverySucceeded
	// goes false the same cycle) — this guard exists for the case that
	// matters anyway: reconcile keeps running against the last known-good
	// a.managed snapshot while discovery has been failing, potentially for
	// a long time, and a disk's Current value or its very presence on the
	// VM may no longer be accurate.
	maxDiscoveryAge time.Duration

	// maxSampleAge/maxSampleGap are the same issue #15 freshness bounds as
	// maxDiscoveryAge, but for the metrics path — derived once in main.go
	// from runtime.collection_interval and threaded into every disk's
	// EvaluationPolicy each reconcile cycle, rather than being a per-disk
	// policy field: freshness is a property of the sampling pipeline, not
	// something a user tunes per disk.
	maxSampleAge time.Duration
	maxSampleGap time.Duration

	// guestPathVerifier confirms a disk's guest device is resolvable and
	// present in /proc/diskstats before discover() admits it to the
	// managed set (issue #4) — a narrow function type, not a dependency on
	// *metrics.GuestSource's concrete type, so this stays swappable the
	// same way every other pipeline stage here is. Wired from
	// (*metrics.GuestSource).VerifyPath in main.go; nil is a valid value
	// used by tests that don't exercise discover() at all.
	guestPathVerifier func(metrics.DiskRef) error

	client    compute.Client
	sampler   *metrics.Sampler
	analyzer  *analyzer.WindowAnalyzer
	decider   decider.Decider
	actuator  actuator.Actuator
	tracker   *state.Tracker
	telemetry *telemetry.Registry
	journal   *journal.Writer
	readiness *health.Readiness

	mu                   sync.Mutex
	managed              []registry.ManagedDisk
	iopsPool             int64
	lastDiscoverySuccess time.Time
}

func (a *Agent) Run(ctx context.Context) error {
	if err := a.discover(ctx); err != nil {
		return fmt.Errorf("initial discovery: %w", err)
	}
	// Reconcile any budget reservation left pending by a previous run that
	// crashed before resolving it (issue #10) — must happen after the
	// first successful discover() (it needs a.managed to route live-state
	// reads) and before the first reconcile() (which needs a trustworthy
	// budget count).
	a.reconcilePendingBudget(ctx)

	// First sample is taken immediately too, so the first reconcile after
	// startup isn't waiting a full CollectionInterval before it has
	// anything at all (it will still be cold-start-empty per NFR-9 either
	// way — Analyze needs a full window, not just one sample).
	if err := a.sampler.Tick(ctx); err != nil {
		log.Printf("sampler: initial tick: %v", err)
		a.readiness.SetSourceReachable(false)
	} else {
		a.readiness.SetSourceReachable(a.hasManagedDisks())
	}

	discoveryTicker := time.NewTicker(a.resolved.Runtime.DiscoveryInterval)
	defer discoveryTicker.Stop()
	collectionTicker := time.NewTicker(a.resolved.Runtime.CollectionInterval)
	defer collectionTicker.Stop()
	reconcileTicker := time.NewTicker(a.resolved.Runtime.ReconcileInterval)
	defer reconcileTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-discoveryTicker.C:
			if err := a.discover(ctx); err != nil {
				log.Printf("discovery: %v", err)
			}
		case <-collectionTicker.C:
			if err := a.sampler.Tick(ctx); err != nil {
				log.Printf("sampler: %v", err)
				a.readiness.SetSourceReachable(false)
			} else {
				a.readiness.SetSourceReachable(a.hasManagedDisks())
			}
		case <-reconcileTicker.C:
			a.reconcile(ctx)
		}
	}
}

// discover is design.md §4.1's slow path: read the VM, apply the
// selection strategy (registry.Select), and hand the resulting set to the
// Sampler. Errors here mark the process not-ready (health.Readiness) but
// never crash the loop — a transient Compute API failure must not take
// down an otherwise-working agent (design.md §6.1's failure-mode table:
// "retry with backoff; decisions queue but never stack up").
func (a *Agent) discover(ctx context.Context) error {
	vm, skipped, err := compute.Discover(ctx, a.client, a.vmName)
	if err != nil {
		a.readiness.SetDiscoverySucceeded(false)
		return err
	}
	for _, s := range skipped {
		log.Printf("discover: skipped disk %s (%s): %v", s.Alias, s.DiskName, s.Reason)
		a.telemetry.RecordDiscoverySkipped(s.Alias, s.Category)
	}

	// An unmatched disks.selected device_name is now a hard error, not a
	// logged omission (issue #5, reopened): registry.Select's "selected"
	// branch never returns a non-nil err alongside a nil missing, and
	// never returns a non-nil missing alongside a nil err — the two
	// possibilities the old code handled separately (fatal vs. "log and
	// continue with a partial set") have collapsed into one. Returning
	// here means a.managed/a.sampler.SetDisks are never reached, so the
	// previous (still valid) managed set is left in place rather than
	// replaced by a partial one — and SetDiscoverySucceeded(false) makes
	// that visible on /readyz, exactly as this issue requires.
	managed, _, err := registry.Select(vm, a.cfg.Disks, a.resolved)
	if err != nil {
		a.readiness.SetDiscoverySucceeded(false)
		return err
	}

	// Local mode's guest-path verification (issue #4): a disk that passed
	// every Compute-API-side check in compute.Discover can still be
	// unmanageable if its /dev/disk/by-id/mws-<deviceName> symlink doesn't
	// resolve, or its kernel device has no /proc/diskstats entry —
	// GuestSource.Sample would otherwise silently emit no sample for it,
	// forever, with no diagnosis beyond an ever-growing
	// metric_staleness_seconds. Verified once here, up front, with a
	// specific reason, instead of left to manifest later as an
	// indistinguishable "no data" stream.
	if a.guestPathVerifier != nil {
		verified := managed[:0]
		for _, m := range managed {
			ref := metrics.DiskRef{DiskID: m.Disk.DiskName, VMName: a.vmName, DeviceName: m.Disk.DeviceName}
			if err := a.guestPathVerifier(ref); err != nil {
				log.Printf("discover: disk %s (%s) failed guest-path verification: %v", m.Disk.Alias, m.Disk.DeviceName, err)
				a.telemetry.RecordDiscoverySkipped(m.Disk.Alias, skipReasonGuestPathUnresolvable)
				continue
			}
			verified = append(verified, m)
		}
		managed = verified
	}

	disks := make([]metrics.DiskRef, len(managed))
	for i, m := range managed {
		disks[i] = metrics.DiskRef{DiskID: m.Disk.DiskName, VMName: a.vmName, DeviceName: m.Disk.DeviceName}
	}
	a.sampler.SetDisks(disks)

	a.mu.Lock()
	a.managed = managed
	a.iopsPool = compute.VMIOPSPool(vm)
	a.lastDiscoverySuccess = time.Now()
	a.mu.Unlock()

	a.readiness.SetDiscoverySucceeded(true)
	// SourceReachable is deliberately not touched here (issue #15): the
	// Compute API and the metrics source are different dependencies even
	// in `local` mode (the guest source reads /proc/diskstats, which can
	// fail — permissions, a missing disk — independent of Compute API
	// reachability). It is driven solely by sampler.Tick()'s own success,
	// in Run().

	// Budget compaction scheduled here (issue #10, per Budget.Compact's
	// own doc comment: "once per discovery cycle... is a natural fit —
	// far less often than every reconcile"). A failure here is logged but
	// not treated as making the budget unhealthy: Compact only ever
	// replaces b.events with the freshly-written set after writeAll
	// succeeds, so a failed attempt leaves the in-memory budget exactly as
	// correct as it was before — only the on-disk file's pruning is
	// deferred to the next cycle, not the budget's own accuracy.
	if err := a.tracker.Compact(time.Now()); err != nil {
		log.Printf("discover: budget compaction failed (will retry next cycle): %v", err)
	}

	return nil
}

// skipReasonGuestPathUnresolvable mirrors the compute package's
// SkipReason* constants (a stable, low-cardinality telemetry label) for
// the one skip category that originates here rather than in
// compute.Discover — the compute package has no concept of a guest
// filesystem to check.
const skipReasonGuestPathUnresolvable = "guest_path_unresolvable"

// hasManagedDisks reports whether any disk currently passed every
// eligibility check (issue #4's "readiness must not report a usable
// metrics path when no managed disk can produce a sample") — read under
// the same lock discover()/reconcile() use for a.managed.
func (a *Agent) hasManagedDisks() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.managed) > 0
}

// reconcile is design.md §4.3, run once per disk per cycle. Every disk is
// analyzed first, in one pass, before any decision is made — VM headroom
// (design.md §4.6) needs every sibling's own Analysis already computed,
// not recomputed per disk.
func (a *Agent) reconcile(ctx context.Context) {
	a.mu.Lock()
	managed := append([]registry.ManagedDisk(nil), a.managed...)
	iopsPool := a.iopsPool
	lastDiscovery := a.lastDiscoverySuccess
	a.mu.Unlock()

	now := time.Now()

	// Polled once per cycle rather than pushed from Tracker itself (issue
	// #10) — keeps Tracker decoupled from health/telemetry the same way
	// every other pure package here stays decoupled from its observers.
	healthy := a.tracker.Healthy()
	a.readiness.SetBudgetHealthy(healthy)
	a.telemetry.RecordBudgetHealthy(healthy)

	// Fail closed on a stale discovery snapshot (issue #15): a.managed
	// keeps whatever discover() last populated even while discovery has
	// since started failing (readiness.SetDiscoverySucceeded already
	// reports that), so reconcile must not keep acting on it indefinitely.
	// lastDiscovery.IsZero() (never discovered) is deliberately included —
	// Run() always discovers once before the first reconcile, so reaching
	// here with a zero value can only mean that first discover() itself
	// failed and this cycle's managed list is empty/stale anyway.
	if age := now.Sub(lastDiscovery); lastDiscovery.IsZero() || age > a.maxDiscoveryAge {
		log.Printf("reconcile: discovery snapshot stale (age=%s, max=%s) — skipping this cycle (issue #15)", age, a.maxDiscoveryAge)
		return
	}

	analyses := make(map[string]analyzer.Analysis, len(managed))
	for _, m := range managed {
		diskID := m.Disk.DiskName
		samples := a.sampler.Samples(diskID)
		ep := toEvaluationPolicy(m.Policy.Advanced, a.maxSampleAge, a.maxSampleGap)
		an := a.analyzer.Analyze(diskID, samples, ep, m.Disk.IOPS, now)
		analyses[diskID] = an
		a.telemetry.RecordStaleness(diskID, an.SampleAge.Seconds())
	}

	// Aggregated across every disk decided this cycle and reported once,
	// after the loop (issue #19) — vm_cap_reached is a VM-wide gauge with
	// no disk label, so setting it per-disk inside the loop let whichever
	// disk was processed last silently overwrite every earlier disk's
	// result.
	anyVMCapReached := false

	for _, m := range managed {
		diskID := m.Disk.DiskName
		an := analyses[diskID]

		// Sibling p95 aggregation (design.md §4.6): summing each sibling's
		// own p95 rather than a joint-timestamp p95 across siblings is the
		// simpler, already-accepted-as-imperfect method — correlated
		// bursty siblings can make this optimistic (design.md §9's open
		// question #6), not something this pass resolves.
		var siblingP95 float64
		for otherID, otherAn := range analyses {
			if otherID != diskID {
				siblingP95 += otherAn.IOPSP95Down
			}
		}
		vm := decider.VMState{IOPSPool: iopsPool, SiblingP95IOPS: siblingP95}

		ep := toEffectivePolicy(m.Policy)
		gState := a.tracker.State(diskID, now)

		d := a.decider.Decide(diskID, m.Disk.IOPS, an, vm, ep, gState, now)
		if d.Action == decider.ActionVMCapReached {
			anyVMCapReached = true
		}

		class := string(m.Disk.Class)
		a.telemetry.RecordDecision(a.vmName, class, d)
		if err := a.journal.Write(journal.NewRecord(now, a.vmName, class, d)); err != nil {
			log.Printf("journal: %v", err)
		}

		if d.Action != decider.ActionScaleUp && d.Action != decider.ActionScaleDown {
			continue
		}
		a.actuate(ctx, m, d, now)
	}

	a.telemetry.RecordVMCapReached(a.vmName, anyVMCapReached)
}

func (a *Agent) actuate(ctx context.Context, m registry.ManagedDisk, d decider.Decision, now time.Time) {
	diskID := m.Disk.DiskName

	if d.Action == decider.ActionScaleDown && !a.allowScaleDown {
		// Defense in depth (issue #14) — PolicyDecider should never
		// produce this, so reaching here means the Decider implementation
		// itself disagrees with this process's own phase gate. Refuse
		// loudly rather than writing, and make it visible the same way
		// any other write-blocking outcome is.
		log.Printf("actuate %s (%s): refusing ActionScaleDown — scale-down is disabled in this build (issue #14)", diskID, m.Disk.DeviceName)
		return
	}

	if !a.tracker.Healthy() {
		// Fail closed (issue #10): a prior budget-persistence failure,
		// this process's whole lifetime, means the durable count can no
		// longer be trusted. Decide would already refuse via
		// budget_exhausted (Tracker.State reports MaxInt), but checking
		// here too avoids even attempting the ReserveChange call below,
		// which would just fail again for the same reason.
		log.Printf("actuate %s (%s): refusing to write — the change budget is unhealthy after a prior persistence failure, fail closed until restart (issue #10)", diskID, m.Disk.DeviceName)
		return
	}

	// Reserve the budget slot before the write is even attempted (issue
	// #10) — never after, which is what let a crash between the
	// platform confirming a write and this process recording it silently
	// undercount a real change against the hard cap. idempotencyKey is
	// generated once here and threaded through both the reservation and
	// the write itself, so a startup reconciliation can always find the
	// exact record a given attempt belongs to.
	idempotencyKey := uuid.NewString()
	if err := a.tracker.ReserveChange(diskID, idempotencyKey, d.Current, d.Target, now); err != nil {
		log.Printf("actuate %s (%s): failed to durably reserve a budget slot before writing — refusing to write: %v", diskID, m.Disk.DeviceName, err)
		return
	}

	target := actuator.Target{
		VMName:     a.vmName,
		DeviceName: m.Disk.DeviceName,
		DiskName:   m.Disk.DiskName,
		Class:      m.Disk.Class,
	}

	a.tracker.BeginResize(diskID)
	applied, err := a.actuator.Apply(ctx, d, target, a.dryRun, idempotencyKey)
	a.tracker.EndResize(diskID)
	if err != nil {
		if a.dryRun {
			// A dry run can never ambiguously succeed server-side: an
			// independent disk's error comes from the platform's own
			// validateOnly call, which by contract never mutates even if
			// the response is lost; a dependent disk's write function
			// returns before ever calling UpdateDependentDisk at all
			// (ADR-007, no server-side dry run exists for it). Safe to
			// release unconditionally — there is no live state to check.
			if rerr := a.tracker.ReleaseChange(idempotencyKey); rerr != nil {
				log.Printf("actuate %s (%s): failed to release the dry-run budget reservation after an error: %v", diskID, m.Disk.DeviceName, rerr)
			}
			log.Printf("actuate %s (%s): %v", diskID, m.Disk.DeviceName, err)
			return
		}

		// A real write's error does not prove the change never landed —
		// the platform may have applied it before the response reached
		// this process (timeout, connection reset, etc). Releasing
		// against d.Current here without checking would reopen exactly
		// the undercounting gap ReserveChange-before-write exists to
		// close: Budget.Release's own contract requires live state
		// verified to still match Before, which an error alone can never
		// establish. Resolve the reservation from a fresh live read
		// instead — the same three-way resolution reconcilePendingBudget
		// already applies at startup for an identically ambiguous
		// situation.
		a.resolveReservationAfterWriteFailure(ctx, m, d, idempotencyKey, now)

		if errors.Is(err, compute.ErrConcurrentModification) {
			// The write path already refused to touch anything (issue
			// #16) — the live value moved since this Decision's snapshot
			// was taken, so d.Target is no longer meaningful. Force a
			// rediscovery now rather than waiting up to DiscoveryInterval,
			// so the next reconcile cycle decides again from a snapshot no
			// older than this moment, instead of retrying the same stale
			// target. A rediscovery failure here is already logged by
			// discover() itself; nothing more to do about it in this path.
			log.Printf("actuate %s (%s): %v — forcing rediscovery before the next decision", diskID, m.Disk.DeviceName, err)
			if derr := a.discover(ctx); derr != nil {
				log.Printf("actuate %s (%s): rediscovery after conflict failed: %v", diskID, m.Disk.DeviceName, derr)
			}
			return
		}
		log.Printf("actuate %s (%s): %v", diskID, m.Disk.DeviceName, err)
		return
	}

	if a.dryRun {
		// Nothing real happened — release the reservation a dry run's own
		// probe still had to make (Reserve happens before Apply
		// regardless of dryRun, so this path stays uniform rather than
		// special-casing dry-run out of the crash-safety flow).
		if rerr := a.tracker.ReleaseChange(idempotencyKey); rerr != nil {
			log.Printf("actuate %s (%s): failed to release the dry-run budget reservation: %v", diskID, m.Disk.DeviceName, rerr)
		}
		log.Printf("[dry-run] %s %s: %d -> %d (%s)", d.Action, m.Disk.DeviceName, d.Current, applied.IOPS, d.Reason)
		return
	}

	// Adopt the verified applied value immediately (issue #16) — waiting
	// for the next discover() to notice would leave a.managed's Current
	// stale for up to DiscoveryInterval, exactly the same class of gap
	// this issue exists to close for external changes.
	a.mu.Lock()
	for i := range a.managed {
		if a.managed[i].Disk.DiskName == diskID {
			a.managed[i].Disk.IOPS = applied.IOPS
			break
		}
	}
	a.mu.Unlock()

	// disk_provisioned_iops only ever moves on a write verified to have
	// landed (issue #19) — this is that verification point, the same one
	// issue #16's comment above just adopted into a.managed.
	a.telemetry.RecordAppliedIOPS(diskID, a.vmName, string(m.Disk.Class), applied.IOPS)

	if err := a.tracker.ConfirmChange(idempotencyKey); err != nil {
		// The write landed (verified above) but confirming that fact
		// durably failed — the reservation stays "pending" on disk, which
		// is still safe (issue #10: pending counts the same as confirmed)
		// and will resolve itself via the next restart's reconciliation.
		log.Printf("actuate %s (%s): failed to durably confirm a successful write — the budget reservation stays pending until the next restart's reconciliation: %v", diskID, m.Disk.DeviceName, err)
	}
	a.tracker.RecordChange(diskID, d.Action == decider.ActionScaleUp, now)
	log.Printf("%s %s: %d -> %d (%s)", d.Action, m.Disk.DeviceName, d.Current, applied.IOPS, d.Reason)
}

// resolveReservationAfterWriteFailure re-reads diskID's live IOPS after a
// real (non-dry-run) actuator.Apply error and resolves idempotencyKey's
// reservation from that reading alone — never from the error, which cannot
// distinguish "nothing was applied" from "applied, but the response never
// reached this process." Exactly the same three-way resolution
// reconcilePendingBudget already applies at startup for an identically
// ambiguous situation (a crash instead of a single failed call), so this
// reuses its logic rather than inventing a second one:
//
//   - live == d.Target: the write landed despite the error. Adopted into
//     a.managed and confirmed the same way a verified success already is
//     (issue #16) — the next cycle's decision must see reality, not a
//     stale Current, and the reservation must count as the real change it
//     was, not sit wrongly released.
//   - live == d.Current: conclusively never landed. Released — this is
//     the one case Budget.Release's own contract (live verified to still
//     match Before) is actually satisfied.
//   - anything else, including a failed read itself: left pending,
//     fail closed. The live value cannot be explained by this attempt
//     alone (most likely an unrelated external change, or the error
//     genuinely was mid-write) — resolving it via reconcilePendingBudget
//     at the next restart, not guessed at here.
func (a *Agent) resolveReservationAfterWriteFailure(ctx context.Context, m registry.ManagedDisk, d decider.Decision, idempotencyKey string, now time.Time) {
	diskID := m.Disk.DiskName

	live, err := a.liveCurrentIOPS(ctx, m)
	if err != nil {
		log.Printf("actuate %s (%s): failed to read live state after a write error — leaving the budget reservation pending (fail closed): %v", diskID, m.Disk.DeviceName, err)
		return
	}

	switch live {
	case d.Target:
		a.mu.Lock()
		for i := range a.managed {
			if a.managed[i].Disk.DiskName == diskID {
				a.managed[i].Disk.IOPS = live
				break
			}
		}
		a.mu.Unlock()
		a.telemetry.RecordAppliedIOPS(diskID, a.vmName, string(m.Disk.Class), live)
		if cerr := a.tracker.ConfirmChange(idempotencyKey); cerr != nil {
			log.Printf("actuate %s (%s): failed to durably confirm a write that landed despite the error: %v", diskID, m.Disk.DeviceName, cerr)
		}
		a.tracker.RecordChange(diskID, d.Action == decider.ActionScaleUp, now)
		log.Printf("actuate %s (%s): write reported an error but live state confirms it landed (%d -> %d) — confirmed, not released", diskID, m.Disk.DeviceName, d.Current, live)
	case d.Current:
		if rerr := a.tracker.ReleaseChange(idempotencyKey); rerr != nil {
			log.Printf("actuate %s (%s): failed to release a reservation confirmed never to have landed: %v", diskID, m.Disk.DeviceName, rerr)
		}
	default:
		log.Printf("actuate %s (%s): live IOPS (%d) after a write error matches neither the reservation's before (%d) nor target (%d) — leaving it pending (fail closed, ambiguous)",
			diskID, m.Disk.DeviceName, live, d.Current, d.Target)
	}
}

// reconcilePendingBudget resolves every budget reservation a previous run
// left unresolved (issue #10) — called once at startup, after the first
// successful discover() has populated a.managed. For each pending
// reservation whose disk is still managed, a fresh live read decides the
// outcome: live current equal to the reservation's Target means the write
// landed (Confirm); equal to Before means it never did (Release); neither
// is ambiguous and is left pending — fail closed — rather than guessed
// at. A disk no longer managed at all (detached/replaced) has nothing to
// verify against, so its reservation is also left as-is: still counted,
// never silently dropped.
func (a *Agent) reconcilePendingBudget(ctx context.Context) {
	pending := a.tracker.PendingChanges()
	if len(pending) == 0 {
		return
	}

	a.mu.Lock()
	managed := append([]registry.ManagedDisk(nil), a.managed...)
	a.mu.Unlock()
	byDiskID := make(map[string]registry.ManagedDisk, len(managed))
	for _, m := range managed {
		byDiskID[m.Disk.DiskName] = m
	}

	for _, ev := range pending {
		m, ok := byDiskID[ev.DiskID]
		if !ok {
			log.Printf("reconcile budget: disk %s has a reservation pending from a previous run but is no longer managed — leaving it counted (fail closed, issue #10)", ev.DiskID)
			continue
		}

		live, err := a.liveCurrentIOPS(ctx, m)
		if err != nil {
			log.Printf("reconcile budget: disk %s: failed to read live state, leaving the reservation pending (fail closed): %v", ev.DiskID, err)
			continue
		}

		switch live {
		case ev.Target:
			if err := a.tracker.ConfirmChange(ev.IdempotencyKey); err != nil {
				log.Printf("reconcile budget: disk %s: failed to confirm a reservation that actually landed: %v", ev.DiskID, err)
			} else {
				log.Printf("reconcile budget: disk %s: reservation from a previous run confirmed — live current matches the target", ev.DiskID)
			}
		case ev.Before:
			if err := a.tracker.ReleaseChange(ev.IdempotencyKey); err != nil {
				log.Printf("reconcile budget: disk %s: failed to release a reservation that never landed: %v", ev.DiskID, err)
			} else {
				log.Printf("reconcile budget: disk %s: reservation from a previous run released — live current still matches the pre-write value", ev.DiskID)
			}
		default:
			log.Printf("reconcile budget: disk %s: live current (%d) matches neither the reservation's before (%d) nor target (%d) — leaving it pending (fail closed, ambiguous)",
				ev.DiskID, live, ev.Before, ev.Target)
		}
	}
}

// liveCurrentIOPS reads diskID's actual current IOPS directly from the
// Compute API, routed by class — used only for startup budget
// reconciliation (issue #10), which must never trust a's own in-memory
// snapshot for this: the whole point is verifying what really happened
// while this process wasn't running.
func (a *Agent) liveCurrentIOPS(ctx context.Context, m registry.ManagedDisk) (int64, error) {
	switch m.Disk.Class {
	case compute.ClassIndependent:
		disk, err := a.client.GetDisk(ctx, m.Disk.DiskName)
		if err != nil {
			return 0, err
		}
		return disk.SpecIOPS, nil
	case compute.ClassDependent:
		vm, err := a.client.GetVirtualMachine(ctx, a.vmName)
		if err != nil {
			return 0, err
		}
		for _, d := range vm.Disks {
			if d.DeviceName == m.Disk.DeviceName {
				return d.IOPS, nil
			}
		}
		return 0, fmt.Errorf("device_name %q not found on VM %q", m.Disk.DeviceName, a.vmName)
	default:
		return 0, fmt.Errorf("disk %q has unknown class %q", m.Disk.DiskName, m.Disk.Class)
	}
}

func toEvaluationPolicy(a config.ResolvedAdvanced, maxSampleAge, maxSampleGap time.Duration) analyzer.EvaluationPolicy {
	return analyzer.EvaluationPolicy{
		ScaleUpWindow:             a.ScaleUpWindow,
		ScaleDownWindow:           a.ScaleDownWindow,
		UtilizationThreshold:      a.UtilizationThreshold,
		QueueDepthThreshold:       a.QueueDepthThreshold,
		LatencyBaselineMultiplier: a.LatencyBaselineMultiplier,
		MaxSampleAge:              maxSampleAge,
		MaxSampleGap:              maxSampleGap,
	}
}

func toEffectivePolicy(p config.ResolvedPolicy) decider.EffectivePolicy {
	return decider.EffectivePolicy{
		MinIOPS:              int64(p.MinIOPS),
		MaxIOPS:              int64(p.MaxIOPS),
		ScaleUpUtilization:   p.Advanced.ScaleUpUtilization,
		ScaleDownUtilization: p.Advanced.ScaleDownUtilization,
		CooldownScaleUp:      p.Advanced.CooldownScaleUp,
		CooldownScaleDown:    p.Advanced.CooldownScaleDown,
		MinChangeIOPS:        int64(p.Advanced.MinChangeIOPS),
		MaxScaleUpRatio:      p.Advanced.MaxScaleUpRatio,
		MaxScaleDownRatio:    p.Advanced.MaxScaleDownRatio,
		MaxChangesPer24h:     p.Advanced.MaxChangesPer24h,
	}
}
