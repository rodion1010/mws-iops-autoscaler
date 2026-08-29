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

	target := actuator.Target{
		VMName:     a.vmName,
		DeviceName: m.Disk.DeviceName,
		DiskName:   m.Disk.DiskName,
		Class:      m.Disk.Class,
	}

	a.tracker.BeginResize(diskID)
	applied, err := a.actuator.Apply(ctx, d, target, a.dryRun, uuid.NewString())
	a.tracker.EndResize(diskID)
	if err != nil {
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

	if err := a.tracker.RecordChange(diskID, d.Action == decider.ActionScaleUp, now); err != nil {
		// The in-memory cooldown already took effect (Tracker.RecordChange
		// updates it before touching the durable budget) — only the 24h
		// count's durability is at risk here, not this cycle's decision.
		log.Printf("record change %s: %v", diskID, err)
	}
	log.Printf("%s %s: %d -> %d (%s)", d.Action, m.Disk.DeviceName, d.Current, applied.IOPS, d.Reason)
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
