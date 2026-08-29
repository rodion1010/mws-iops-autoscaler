package main

import (
	"context"
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
	iopsPool int64
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
	}

	managed, missing, err := registry.Select(vm, a.cfg.Disks, a.resolved)
	if err != nil {
		a.readiness.SetDiscoverySucceeded(false)
		return err
	}
	for _, name := range missing {
		log.Printf("discover: disks.selected device_name %q not found on the VM", name)
	}

	disks := make([]metrics.DiskRef, len(managed))
	for i, m := range managed {
		disks[i] = metrics.DiskRef{DiskID: m.Disk.DiskName, VMName: a.vmName, DeviceName: m.Disk.DeviceName}
	}
	a.sampler.SetDisks(disks)

	a.mu.Lock()
	a.managed = managed
	a.iopsPool = compute.VMIOPSPool(vm)
	a.mu.Unlock()

	a.readiness.SetDiscoverySucceeded(true)
	a.readiness.SetSourceReachable(true) // the same call that just proved the Compute API reachable also proves the metrics path exists in `local` mode (guest source needs no network)
	return nil
}

// reconcile is design.md §4.3, run once per disk per cycle. Every disk is
// analyzed first, in one pass, before any decision is made — VM headroom
// (design.md §4.6) needs every sibling's own Analysis already computed,
// not recomputed per disk.
func (a *Agent) reconcile(ctx context.Context) {
	a.mu.Lock()
	managed := append([]registry.ManagedDisk(nil), a.managed...)
	iopsPool := a.iopsPool
	a.mu.Unlock()

	now := time.Now()

	analyses := make(map[string]analyzer.Analysis, len(managed))
	for _, m := range managed {
		diskID := m.Disk.DiskName
		samples := a.sampler.Samples(diskID)
		ep := toEvaluationPolicy(m.Policy.Advanced)
		analyses[diskID] = a.analyzer.Analyze(diskID, samples, ep, m.Disk.IOPS)
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
		log.Printf("actuate %s (%s): %v", diskID, m.Disk.DeviceName, err)
		return
	}

	if a.dryRun {
		log.Printf("[dry-run] %s %s: %d -> %d (%s)", d.Action, m.Disk.DeviceName, d.Current, applied.IOPS, d.Reason)
		return
	}

	if err := a.tracker.RecordChange(diskID, d.Action == decider.ActionScaleUp, now); err != nil {
		// The in-memory cooldown already took effect (Tracker.RecordChange
		// updates it before touching the durable budget) — only the 24h
		// count's durability is at risk here, not this cycle's decision.
		log.Printf("record change %s: %v", diskID, err)
	}
	log.Printf("%s %s: %d -> %d (%s)", d.Action, m.Disk.DeviceName, d.Current, applied.IOPS, d.Reason)
}

func toEvaluationPolicy(a config.ResolvedAdvanced) analyzer.EvaluationPolicy {
	return analyzer.EvaluationPolicy{
		ScaleUpWindow:             a.ScaleUpWindow,
		ScaleDownWindow:           a.ScaleDownWindow,
		UtilizationThreshold:      a.UtilizationThreshold,
		QueueDepthThreshold:       a.QueueDepthThreshold,
		LatencyBaselineMultiplier: a.LatencyBaselineMultiplier,
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
