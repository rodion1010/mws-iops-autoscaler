package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
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

// Agent coordinates discovery, sampling, analysis, decisions, writes, state, and observability.
type Agent struct {
	cfg      *config.Config
	resolved *config.ResolvedConfig
	vmName   string
	dryRun   bool

	// Maximum age of the last successful discovery before reconcile fails closed.
	maxDiscoveryAge time.Duration

	// Sampling freshness bounds are process-wide, not per-disk policy.
	maxSampleAge time.Duration
	maxSampleGap time.Duration

	// Optional local-mode check that the guest device resolves and appears in diskstats.
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

	mu       sync.Mutex
	managed  []registry.ManagedDisk
	degraded []registry.Degradation
	// unmanaged is every other disk discovered on the VM this cycle — never
	// decided or actuated on, sampled and analyzed in reconcile() purely so
	// its measured consumption folds into siblingP95 (design.md §4.6: VM
	// headroom is "other disks", not "other managed disks" — US-009).
	unmanaged            []compute.StorageDisk
	iopsPool             int64
	lastDiscoverySuccess time.Time

	// Cleared immediately on discovery failure; separate from age-based staleness.
	managedValid bool

	// Successful discovery generation; a mid-cycle change aborts remaining decisions.
	generation int64
}

func (a *Agent) Run(ctx context.Context) error {
	if err := a.discover(ctx); err != nil {
		return fmt.Errorf("initial discovery: %w", err)
	}
	// Resolve crash-left budget reservations before the first decision cycle.
	a.reconcilePendingBudget(ctx)

	// Prime sampling immediately; window readiness still prevents cold-start actions.
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

// discover refreshes managed disks and invalidates the snapshot on fatal discovery errors.
//
// An unresolved disks.selected/disks.exclude name (registry.Degradation) is
// fatal only on the very first discovery attempt of this process's life —
// managing a silently partial set with no prior successful discovery to
// fall back to would diverge from operator intent with nothing to show for
// it (docs/design.md §4.1). Once at least one discovery has already
// succeeded, the same mismatch degrades only the unresolved name: the rest
// of the managed set keeps being managed, reported via
// health.Readiness.SetDegradedDisks and the
// iops_autoscaler_disk_configured_unmanaged gauge, neither of which flips
// /readyz's binary verdict (docs/configuration.md §4.2).
func (a *Agent) discover(ctx context.Context) error {
	a.mu.Lock()
	coldStart := a.lastDiscoverySuccess.IsZero()
	a.mu.Unlock()

	vm, skipped, err := compute.Discover(ctx, a.client, a.vmName)
	if err != nil {
		a.readiness.SetDiscoverySucceeded(false)
		a.invalidateManaged()
		return err
	}
	for _, s := range skipped {
		log.Printf("discover: skipped disk %s (%s): %v", s.Alias, s.DiskName, s.Reason)
		a.telemetry.RecordDiscoverySkipped(s.Alias, s.Category)
	}

	managed, degraded, err := registry.Select(vm, a.cfg.Disks, a.resolved)
	if err != nil {
		a.readiness.SetDiscoverySucceeded(false)
		a.invalidateManaged()
		return err
	}
	if len(degraded) > 0 && coldStart {
		a.readiness.SetDiscoverySucceeded(false)
		a.invalidateManaged()
		return fmt.Errorf("disks.%s: unresolved device_name(s) on the first discovery attempt: %s — refusing to manage a partial set before any discovery has succeeded (docs/design.md §4.1)",
			a.cfg.Disks.Mode, joinDegraded(degraded))
	}

	// Exclude disks whose guest path cannot produce metrics.
	if a.guestPathVerifier != nil {
		verified := managed[:0]
		for _, m := range managed {
			if err := a.guestPathVerifier(a.diskRef(m)); err != nil {
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
		disks[i] = a.diskRef(m)
	}

	// Every disk this cycle found on the VM but did not select into managed
	// still shares the same aggregate IOPS pool — sampled and analyzed
	// alongside the managed set (reconcile) so its consumption is not
	// silently treated as zero when computing another disk's headroom.
	managedNames := make(map[string]bool, len(managed))
	for _, m := range managed {
		managedNames[m.Disk.DiskName] = true
	}
	var unmanaged []compute.StorageDisk
	for _, d := range vm.Disks {
		if !managedNames[d.DiskName] {
			unmanaged = append(unmanaged, d)
			disks = append(disks, a.diskRefForDisk(d))
		}
	}
	a.sampler.SetDisks(disks)

	a.mu.Lock()
	a.managed = managed
	a.unmanaged = unmanaged
	previouslyDegraded := a.degraded
	a.degraded = degraded
	a.iopsPool = compute.VMIOPSPool(vm)
	a.lastDiscoverySuccess = time.Now()
	a.managedValid = true
	a.generation++
	a.mu.Unlock()

	a.readiness.SetDiscoverySucceeded(true)
	// Compute API and metric-source reachability are independent readiness signals.
	a.reportDegraded(degraded, previouslyDegraded)

	// Compact opportunistically; failure leaves the in-memory budget intact.
	if err := a.tracker.Compact(time.Now()); err != nil {
		log.Printf("discover: budget compaction failed (will retry next cycle): %v", err)
	}

	return nil
}

// joinDegraded formats a degraded set for the cold-start fatal error and
// the runtime WARN log alike, one entry per unresolved name.
func joinDegraded(degraded []registry.Degradation) string {
	parts := make([]string, len(degraded))
	for i, d := range degraded {
		parts[i] = fmt.Sprintf("%s (%s)", d.DeviceName, d.Reason)
	}
	return strings.Join(parts, ", ")
}

// reportDegraded publishes this cycle's degraded set to /readyz and the
// gauge every cycle — including empty, which is what lets a disk that
// resolves again disappear from both instead of leaving a stale entry
// behind — and logs a WARN only when the set actually changed, not on
// every cycle it stays the same.
func (a *Agent) reportDegraded(current, previous []registry.Degradation) {
	healthDegraded := make([]health.DegradedDisk, len(current))
	metricDegraded := make(map[string]string, len(current))
	for i, d := range current {
		healthDegraded[i] = health.DegradedDisk{DeviceName: d.DeviceName, Reason: d.Reason}
		metricDegraded[d.DeviceName] = d.Reason
	}
	a.readiness.SetDegradedDisks(healthDegraded)
	a.telemetry.SyncDiskConfiguredUnmanaged(metricDegraded)

	if !degradedEqual(current, previous) {
		if len(current) > 0 {
			log.Printf("discover: degraded (managing the rest of the set): %s", joinDegraded(current))
		} else {
			log.Printf("discover: previously degraded disk(s) resolved, no longer degraded")
		}
	}
}

func degradedEqual(a, b []registry.Degradation) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// journalDiscoveryStale emits one no-action record per disk without fabricating analysis inputs.
func (a *Agent) journalDiscoveryStale(managed []registry.ManagedDisk, now time.Time) {
	for _, m := range managed {
		d := decider.Decision{
			DiskID:  m.Disk.DiskName,
			Current: m.Disk.IOPS,
			Target:  m.Disk.IOPS,
			Action:  decider.ActionNoAction,
			Reason:  "discovery_stale",
		}
		class := string(m.Disk.Class)
		if err := a.journal.Write(journal.NewRecord(now, a.vmName, class, d)); err != nil {
			log.Printf("journal: %v", err)
		}
	}
}

func (a *Agent) diskRef(m registry.ManagedDisk) metrics.DiskRef {
	return a.diskRefForDisk(m.Disk)
}

// diskRefForDisk is diskRef's non-managed counterpart, for sampling an
// unmanaged sibling purely toward VM headroom (never toward a Decision).
func (a *Agent) diskRefForDisk(d compute.StorageDisk) metrics.DiskRef {
	return metrics.DiskRef{
		DiskID:        d.DiskName,
		VMName:        a.vmName,
		DeviceName:    d.DeviceName,
		IncarnationID: d.IncarnationID,
	}
}

// invalidateManaged prevents reconcile from using cached disks while retaining them for diagnostics.
func (a *Agent) invalidateManaged() {
	a.mu.Lock()
	a.managedValid = false
	a.mu.Unlock()
}

const skipReasonGuestPathUnresolvable = "guest_path_unresolvable"

// hasManagedDisks distinguishes reachable metrics from usable disk coverage.
func (a *Agent) hasManagedDisks() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.managed) > 0
}

// reconcile analyzes all disks first so VM headroom uses one cycle-wide snapshot.
func (a *Agent) reconcile(ctx context.Context) {
	a.mu.Lock()
	managed := append([]registry.ManagedDisk(nil), a.managed...)
	unmanaged := append([]compute.StorageDisk(nil), a.unmanaged...)
	iopsPool := a.iopsPool
	lastDiscovery := a.lastDiscoverySuccess
	managedValid := a.managedValid
	startGeneration := a.generation
	a.mu.Unlock()

	now := time.Now()

	healthy := a.tracker.Healthy()
	a.readiness.SetBudgetHealthy(healthy)
	a.telemetry.RecordBudgetHealthy(healthy)

	if !managedValid {
		log.Printf("reconcile: managed snapshot invalidated by the last discover() call failing — skipping this cycle until the next successful discovery")
		a.journalDiscoveryStale(managed, now)
		return
	}

	// The age guard also catches a stuck discovery loop whose last call succeeded.
	if age := now.Sub(lastDiscovery); lastDiscovery.IsZero() || age > a.maxDiscoveryAge {
		log.Printf("reconcile: discovery snapshot stale (age=%s, max=%s) — skipping this cycle (issue #15)", age, a.maxDiscoveryAge)
		a.journalDiscoveryStale(managed, now)
		return
	}

	analyses := make(map[string]analyzer.Analysis, len(managed)+len(unmanaged))
	for _, m := range managed {
		diskID := m.Disk.DiskName
		samples := a.sampler.Samples(a.diskRef(m))
		ep := toEvaluationPolicy(m.Policy.Advanced, a.maxSampleAge, a.maxSampleGap)
		// Baselines use the same incarnation-aware key as sample storage.
		an := a.analyzer.Analyze(m.Disk.IdentityKey(), samples, ep, m.Disk.IOPS, now)
		// Keep exported analysis identity human-readable; IdentityKey is internal state only.
		an.DiskID = diskID
		analyses[diskID] = an
		// Publish staleness only after the first real sample.
		if an.HasData {
			a.telemetry.RecordStaleness(diskID, an.SampleAge.Seconds())
		}
	}

	// Unmanaged siblings are analyzed too, but only ever feed the headroom
	// sum below — never decided, actuated, journaled, or published to
	// per-disk staleness. registry.Select never resolves a per-disk policy
	// override for a disk outside the managed set, so the VM-wide default
	// stands in for every one of them. Guarded on a non-empty slice so a
	// caller with no unmanaged siblings this cycle never has to carry a
	// resolved config just to reach this line.
	if len(unmanaged) > 0 {
		unmanagedEP := toEvaluationPolicy(a.resolved.VMPolicy.Advanced, a.maxSampleAge, a.maxSampleGap)
		for _, d := range unmanaged {
			diskID := d.DiskName
			samples := a.sampler.Samples(a.diskRefForDisk(d))
			an := a.analyzer.Analyze(d.IdentityKey(), samples, unmanagedEP, d.IOPS, now)
			an.DiskID = diskID
			analyses[diskID] = an
		}
	}

	// Aggregate the VM-wide gauges once to avoid last-disk-wins.
	anyVMCapReached := false
	// minIOPSSum is a static-configuration check, unrelated to anyVMCapReached's
	// dynamic demand signal: it can be non-zero even on a VM with zero load,
	// whenever the operator's own per-disk min_iops floors already add up to
	// more than this VM's real pool.
	var minIOPSSum int64

	for _, m := range managed {
		diskID := m.Disk.DiskName
		an := analyses[diskID]

		// Sum sibling p95s over every disk on the VM, managed or not
		// (docs/design.md §4.6: "other disks", not "other managed disks" —
		// US-009); correlated bursts remain a documented approximation.
		// A sibling with no fresh sample yet contributes IOPSP95Down's zero
		// value to the sum rather than its real, unknown consumption
		// (issue #56) — siblingDemandUnknown flags that gap so Decide can
		// fail closed instead of approving scale-up against headroom that
		// might already be spent.
		var siblingP95 float64
		siblingDemandUnknown := false
		for otherID, otherAn := range analyses {
			if otherID != diskID {
				siblingP95 += otherAn.IOPSP95Down
				if !otherAn.HasData || !otherAn.Fresh {
					siblingDemandUnknown = true
				}
			}
		}
		vm := decider.VMState{IOPSPool: iopsPool, SiblingP95IOPS: siblingP95, SiblingDemandUnknown: siblingDemandUnknown}

		ep := toEffectivePolicy(m.Policy)
		minIOPSSum += ep.MinIOPS
		// Cooldown and budget state must not survive disk replacement.
		gState := a.tracker.State(m.Disk.IdentityKey(), now)

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

		// Rediscovery success changes generation; failure invalidates the snapshot.
		// Either outcome aborts the remaining cycle.
		a.mu.Lock()
		currentGeneration := a.generation
		stillValid := a.managedValid
		a.mu.Unlock()
		if currentGeneration != startGeneration || !stillValid {
			log.Printf("reconcile: managed snapshot superseded or invalidated mid-cycle (forced rediscovery after a conflict or incarnation mismatch) — aborting the rest of this reconcile cycle rather than act on now-stale data")
			return
		}
	}

	a.telemetry.RecordVMCapReached(a.vmName, anyVMCapReached)

	// Unconditional, like RecordVMCapReached above: an empty managed set
	// sums to 0, which is never > iopsPool, so this correctly clears a
	// previously-set gauge back to false rather than leaving it stale.
	floorExceedsPool := minIOPSSum > iopsPool
	if floorExceedsPool {
		log.Printf("reconcile: this VM's managed disks' configured min_iops floors sum to %d, which exceeds its real IOPS pool of %d — these floors cannot all be honored simultaneously regardless of load", minIOPSSum, iopsPool)
	}
	a.telemetry.RecordVMMinIOPSFloorExceedsPool(a.vmName, floorExceedsPool)
}

func (a *Agent) actuate(ctx context.Context, m registry.ManagedDisk, d decider.Decision, now time.Time) {
	diskID := m.Disk.DiskName
	// Tracker state uses incarnation-aware identity; logs use the resource name.
	identity := m.Disk.IdentityKey()

	if !a.tracker.Healthy() {
		// Fail closed while durable budget state is unhealthy.
		log.Printf("actuate %s (%s): refusing to write — the change budget is unhealthy after a prior persistence failure, fail closed until restart (issue #10)", diskID, m.Disk.DeviceName)
		return
	}

	// Reserve durably before the write and reuse its idempotency key.
	idempotencyKey := uuid.NewString()
	if err := a.tracker.ReserveChange(identity, idempotencyKey, d.Current, d.Target, now); err != nil {
		log.Printf("actuate %s (%s): failed to durably reserve a budget slot before writing — refusing to write: %v", diskID, m.Disk.DeviceName, err)
		return
	}

	target := actuator.Target{
		VMName:        a.vmName,
		DeviceName:    m.Disk.DeviceName,
		DiskName:      m.Disk.DiskName,
		Class:         m.Disk.Class,
		IncarnationID: m.Disk.IncarnationID,
	}

	a.tracker.BeginResize(identity)
	applied, err := a.actuator.Apply(ctx, d, target, a.dryRun, idempotencyKey)
	a.tracker.EndResize(identity)
	if err != nil {
		// A conflict proves the managed snapshot is stale in dry-run too: the write path
		// runs its fresh-read preconditions there as well.
		staleSnapshot := errors.Is(err, compute.ErrConcurrentModification) || errors.Is(err, compute.ErrIncarnationMismatch)
		if a.dryRun && !staleSnapshot {
			// Dry-run errors cannot hide a mutation, so the reservation is releasable.
			if rerr := a.tracker.ReleaseChange(idempotencyKey); rerr != nil {
				log.Printf("actuate %s (%s): failed to release the dry-run budget reservation after an error: %v", diskID, m.Disk.DeviceName, rerr)
			}
			log.Printf("actuate %s (%s): %v", diskID, m.Disk.DeviceName, err)
			return
		}

		// A typed conflict proves no mutation, so the reservation is releasable.
		if errors.Is(err, compute.ErrConcurrentModification) {
			if rerr := a.tracker.ReleaseChange(idempotencyKey); rerr != nil {
				log.Printf("actuate %s (%s): failed to release the reservation for a conflict-refused write — no mutation was applied (client-side pre-write refusal or server-side etag rejection): %v", diskID, m.Disk.DeviceName, rerr)
			}
			// Refresh immediately; the target is no longer meaningful against live state.
			log.Printf("actuate %s (%s): %v — forcing rediscovery before the next decision", diskID, m.Disk.DeviceName, err)
			if derr := a.discover(ctx); derr != nil {
				log.Printf("actuate %s (%s): rediscovery after conflict failed: %v", diskID, m.Disk.DeviceName, derr)
			}
			return
		}
		if errors.Is(err, compute.ErrIncarnationMismatch) {
			// No mutation was sent.
			if rerr := a.tracker.ReleaseChange(idempotencyKey); rerr != nil {
				log.Printf("actuate %s (%s): failed to release the reservation for a write refused against a replacement disk: %v", diskID, m.Disk.DeviceName, rerr)
			}
			// Replacement invalidates the cycle-wide disk and VM snapshot.
			log.Printf("actuate %s (%s): %v — forcing rediscovery to pick up the replacement before the next decision", diskID, m.Disk.DeviceName, err)
			if derr := a.discover(ctx); derr != nil {
				log.Printf("actuate %s (%s): rediscovery after incarnation mismatch failed: %v", diskID, m.Disk.DeviceName, derr)
			}
			return
		}

		// An unclassified error may be post-write; resolve it from fresh state before releasing budget.
		a.resolveReservationAfterWriteFailure(ctx, m, d, idempotencyKey, now)
		log.Printf("actuate %s (%s): %v", diskID, m.Disk.DeviceName, err)
		return
	}

	if a.dryRun {
		if rerr := a.tracker.ReleaseChange(idempotencyKey); rerr != nil {
			log.Printf("actuate %s (%s): failed to release the dry-run budget reservation: %v", diskID, m.Disk.DeviceName, rerr)
		}
		log.Printf("[dry-run] %s %s: %d -> %d (%s)", d.Action, m.Disk.DeviceName, d.Current, applied.IOPS, d.Reason)
		return
	}

	// Adopt the verified value immediately instead of waiting for discovery.
	a.mu.Lock()
	for i := range a.managed {
		if a.managed[i].Disk.DiskName == diskID {
			a.managed[i].Disk.IOPS = applied.IOPS
			break
		}
	}
	a.mu.Unlock()

	a.telemetry.RecordAppliedIOPS(diskID, a.vmName, string(m.Disk.Class), applied.IOPS)

	if err := a.tracker.ConfirmChange(idempotencyKey); err != nil {
		// Pending still counts against budget and is reconciled after restart.
		log.Printf("actuate %s (%s): failed to durably confirm a successful write — the budget reservation stays pending until the next restart's reconciliation: %v", diskID, m.Disk.DeviceName, err)
	}
	a.tracker.RecordChange(identity, d.Action == decider.ActionScaleUp, now)
	log.Printf("%s %s: %d -> %d (%s)", d.Action, m.Disk.DeviceName, d.Current, applied.IOPS, d.Reason)
}

// resolveReservationAfterWriteFailure resolves an ambiguous write from fresh live state.
// Target confirms and adopts, Current releases, and anything else remains pending.
func (a *Agent) resolveReservationAfterWriteFailure(ctx context.Context, m registry.ManagedDisk, d decider.Decision, idempotencyKey string, now time.Time) {
	diskID := m.Disk.DiskName
	identity := m.Disk.IdentityKey()

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
		a.tracker.RecordChange(identity, d.Action == decider.ActionScaleUp, now)
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

// reconcilePendingBudget resolves crash-left reservations from fresh live state.
// Target confirms, Before releases, and ambiguity remains pending.
func (a *Agent) reconcilePendingBudget(ctx context.Context) {
	pending := a.tracker.PendingChanges()
	if len(pending) == 0 {
		return
	}

	a.mu.Lock()
	managed := append([]registry.ManagedDisk(nil), a.managed...)
	a.mu.Unlock()
	byIdentity := make(map[string]registry.ManagedDisk, len(managed))
	for _, m := range managed {
		byIdentity[m.Disk.IdentityKey()] = m
	}

	for _, ev := range pending {
		m, ok := byIdentity[ev.DiskID]
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

// liveCurrentIOPS reads current IOPS from the Compute API by disk class.
func (a *Agent) liveCurrentIOPS(ctx context.Context, m registry.ManagedDisk) (int64, error) {
	switch m.Disk.Class {
	case compute.ClassIndependent:
		disk, err := a.client.GetDisk(ctx, m.Disk.DiskName)
		if err != nil {
			return 0, err
		}
		if err := verifyLiveIncarnation(m.Disk.DiskName, m.Disk.IncarnationID, disk.IncarnationID); err != nil {
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
				// The VM response carries no disk incarnation, so read the attached disk and take
				// both identity and value from that one response. An empty expected incarnation
				// skips only the identity comparison, not this read.
				disk, err := a.client.GetDisk(ctx, d.DiskName)
				if err != nil {
					return 0, err
				}
				if err := verifyLiveIncarnation(m.Disk.DiskName, m.Disk.IncarnationID, disk.IncarnationID); err != nil {
					return 0, err
				}
				return disk.SpecIOPS, nil
			}
		}
		return 0, fmt.Errorf("device_name %q not found on VM %q", m.Disk.DeviceName, a.vmName)
	default:
		return 0, fmt.Errorf("disk %q has unknown class %q", m.Disk.DiskName, m.Disk.Class)
	}
}

// verifyLiveIncarnation fails closed: a known expected incarnation needs a present, equal live one.
// An empty expected ID (a reservation recorded without identity) skips the check.
func verifyLiveIncarnation(diskName, expected, live string) error {
	if expected == "" || expected == live {
		return nil
	}
	if live == "" {
		return fmt.Errorf("disk %q has no live incarnation to compare with the reservation's %q — its live state cannot resolve this reservation", diskName, expected)
	}
	return fmt.Errorf("disk %q live incarnation %q differs from the reservation's %q — a different resource was created under the same name; its live state cannot resolve this reservation", diskName, live, expected)
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
