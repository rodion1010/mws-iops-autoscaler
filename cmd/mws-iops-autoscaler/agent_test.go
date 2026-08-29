package main

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// recordingActuator records every Apply call it receives — used to prove
// the Agent never even calls the Actuator for a refused action, not just
// that the result is discarded.
type recordingActuator struct {
	calls []decider.Decision
}

func (r *recordingActuator) Apply(_ context.Context, d decider.Decision, _ actuator.Target, _ bool, _ string) (actuator.Applied, error) {
	r.calls = append(r.calls, d)
	return actuator.Applied{DiskID: d.DiskID, IOPS: d.Target}, nil
}

func newTestAgent(t *testing.T, allowScaleDown bool, act actuator.Actuator) (*Agent, *state.Tracker) {
	t.Helper()
	budget, err := state.LoadBudget(t.TempDir() + "/budget.jsonl")
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	tracker := state.NewTracker(budget)
	return &Agent{
		vmName:         "vm-1",
		allowScaleDown: allowScaleDown,
		actuator:       act,
		tracker:        tracker,
		telemetry:      telemetry.NewRegistry(),
		journal:        journal.NewWriter(discard{}),
	}, tracker
}

// scrapeAgentTelemetry renders a's telemetry registry the same way the
// real /metrics endpoint would, for tests that need to assert on exported
// series rather than internal state.
func scrapeAgentTelemetry(t *testing.T, a *Agent) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	a.telemetry.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func dependentDisk() registry.ManagedDisk {
	return registry.ManagedDisk{Disk: compute.StorageDisk{
		Alias: "bootdisk", DeviceName: "bootdisk", Class: compute.ClassDependent, DiskName: "vm-1-boot",
	}}
}

func independentDisk() registry.ManagedDisk {
	return registry.ManagedDisk{Disk: compute.StorageDisk{
		Alias: "datadisk", DeviceName: "datadisk", Class: compute.ClassIndependent, DiskName: "vm-1-data",
	}}
}

func TestActuate_refusesScaleDownWhenDisabled_dependent(t *testing.T) {
	act := &recordingActuator{}
	a, _ := newTestAgent(t, false, act)
	d := decider.Decision{DiskID: "vm-1-boot", Action: decider.ActionScaleDown, Current: 2000, Target: 1000}
	a.actuate(context.Background(), dependentDisk(), d, time.Now())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — scale-down must be refused before reaching the Actuator", len(act.calls))
	}
}

func TestActuate_refusesScaleDownWhenDisabled_independent(t *testing.T) {
	act := &recordingActuator{}
	a, _ := newTestAgent(t, false, act)
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleDown, Current: 2000, Target: 1000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — scale-down must be refused before reaching the Actuator", len(act.calls))
	}
}

func TestActuate_scaleUpUnaffectedByGate(t *testing.T) {
	act := &recordingActuator{}
	a, tracker := newTestAgent(t, false, act)
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())
	if len(act.calls) != 1 {
		t.Fatalf("Actuator.Apply called %d times, want 1 for an allowed ScaleUp", len(act.calls))
	}
	if got := tracker.State("vm-1-data", time.Now()).LastChangeUp; got.IsZero() {
		t.Error("RecordChange was not called for a successful scale-up")
	}
}

func TestActuate_allowScaleDownTrueReachesActuator(t *testing.T) {
	act := &recordingActuator{}
	a, _ := newTestAgent(t, true, act)
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleDown, Current: 2000, Target: 1000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())
	if len(act.calls) != 1 {
		t.Errorf("Actuator.Apply called %d times, want 1 when scale-down is explicitly allowed", len(act.calls))
	}
}

// stubClient is a minimal compute.Client for exercising discover() end to
// end in a test — it only needs to satisfy whatever a trivial
// auto-discovery VM touches. disks, when set, backs GetDisk for whichever
// resource names the VM's own Disks entries name.
type stubClient struct {
	getVMCalls int
	vm         *compute.VirtualMachine
	disks      map[string]*compute.Disk
	// getVMErr, if set, is returned by GetVirtualMachine instead of a
	// value — simulates discover() itself failing (a transient Compute
	// API error), distinct from every other stub failure mode here which
	// simulates a write-path problem.
	getVMErr error
}

func (s *stubClient) GetVirtualMachine(_ context.Context, _ string) (*compute.VirtualMachine, error) {
	s.getVMCalls++
	if s.getVMErr != nil {
		return nil, s.getVMErr
	}
	cp := *s.vm
	cp.Disks = append([]compute.StorageDisk(nil), s.vm.Disks...)
	return &cp, nil
}
func (s *stubClient) GetVmType(_ context.Context, _ string) (*compute.VmType, error) {
	return &compute.VmType{VCPUCount: 2}, nil
}
func (s *stubClient) GetDisk(_ context.Context, name string) (*compute.Disk, error) {
	if d, ok := s.disks[name]; ok {
		return d, nil
	}
	return nil, errors.New("stub: unused")
}
func (s *stubClient) UpdateIndependentDisk(_ context.Context, _ compute.IndependentUpdateRequest) (*compute.Disk, error) {
	return nil, errors.New("stub: unused")
}
func (s *stubClient) UpdateDependentDisk(_ context.Context, _ compute.DependentUpdateRequest) (*compute.VirtualMachine, error) {
	return nil, errors.New("stub: unused")
}

// erroringActuator always fails with a caller-supplied error — used to
// simulate the write path refusing a stale/conflicting Decision.
type erroringActuator struct{ err error }

func (e erroringActuator) Apply(context.Context, decider.Decision, actuator.Target, bool, string) (actuator.Applied, error) {
	return actuator.Applied{}, e.err
}

// newDiscoverableTestAgent is newTestAgent plus everything discover()
// itself needs (client/cfg/resolved/sampler/readiness) — for tests that
// must observe discover() actually running, not just that actuate()
// returned without error.
func newDiscoverableTestAgent(t *testing.T, act actuator.Actuator, client compute.Client) *Agent {
	t.Helper()
	budget, err := state.LoadBudget(t.TempDir() + "/budget.jsonl")
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	sampler, err := metrics.NewSampler(emptySource{}, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	return &Agent{
		vmName:         "vm-1",
		allowScaleDown: true,
		cfg:            &config.Config{Disks: config.DisksSpec{Mode: "auto-discovery"}},
		resolved:       &config.ResolvedConfig{},
		client:         client,
		sampler:        sampler,
		actuator:       act,
		tracker:        state.NewTracker(budget),
		telemetry:      telemetry.NewRegistry(),
		journal:        journal.NewWriter(discard{}),
		readiness:      &health.Readiness{},
	}
}

func TestActuate_successAdoptsAppliedValueImmediately(t *testing.T) {
	// issue #16: a successful write must not leave a.managed's Current
	// stale until the next discover() — actuate() adopts the verified
	// applied value itself.
	act := &recordingActuator{}
	a, _ := newTestAgent(t, true, act)
	a.managed = []registry.ManagedDisk{independentDisk()}

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := a.managed[0].Disk.IOPS; got != 2000 {
		t.Errorf("managed disk IOPS after a successful write = %d, want 2000 (the applied value, adopted immediately)", got)
	}
}

// TestActuate_telemetry_successMovesProvisionedIOPS is issue #19's
// verified-write scenario at the full actuate() level: only a write
// actuate() itself confirms landed may move disk_provisioned_iops.
func TestActuate_telemetry_successMovesProvisionedIOPS(t *testing.T) {
	act := &recordingActuator{}
	a, _ := newTestAgent(t, true, act)
	a.managed = []registry.ManagedDisk{independentDisk()}

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.telemetry.RecordDecision(a.vmName, string(independentDisk().Disk.Class), d)
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	body := scrapeAgentTelemetry(t, a)
	if !strings.Contains(body, `iops_autoscaler_disk_provisioned_iops{class="independent",disk="vm-1-data",vm="vm-1"} 2000`) {
		t.Errorf("provisioned_iops did not move to the verified applied value after a successful write:\n%s", body)
	}
}

// TestActuate_telemetry_dryRunNeverMovesProvisionedIOPS covers the dry-run
// half of the same acceptance criterion — RecordDecision's own Current
// value must be the only thing on the wire, never Target, and actuate()
// must add nothing to it for a dry run.
func TestActuate_telemetry_dryRunNeverMovesProvisionedIOPS(t *testing.T) {
	act := &recordingActuator{}
	a, _ := newTestAgent(t, true, act)
	a.dryRun = true

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.telemetry.RecordDecision(a.vmName, string(independentDisk().Disk.Class), d)
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	body := scrapeAgentTelemetry(t, a)
	if !strings.Contains(body, `iops_autoscaler_disk_provisioned_iops{class="independent",disk="vm-1-data",vm="vm-1"} 1000`) {
		t.Errorf("provisioned_iops moved off Current for a dry run:\n%s", body)
	}
	if strings.Contains(body, `iops_autoscaler_disk_provisioned_iops{class="independent",disk="vm-1-data",vm="vm-1"} 2000`) {
		t.Error("provisioned_iops reads the proposed target after a dry run — dry-run must never apply anything")
	}
}

// TestActuate_telemetry_failedWriteNeverMovesProvisionedIOPS is the same
// acceptance criterion's failed-write half.
func TestActuate_telemetry_failedWriteNeverMovesProvisionedIOPS(t *testing.T) {
	act := &erroringActuatorRecording{err: errors.New("simulated write failure")}
	a, _ := newTestAgent(t, true, act)
	a.client = &stubClient{disks: map[string]*compute.Disk{
		"vm-1-data": {Name: "vm-1-data", SpecIOPS: 1000}, // live still matches Current
	}}

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.telemetry.RecordDecision(a.vmName, string(independentDisk().Disk.Class), d)
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	body := scrapeAgentTelemetry(t, a)
	if !strings.Contains(body, `iops_autoscaler_disk_provisioned_iops{class="independent",disk="vm-1-data",vm="vm-1"} 1000`) {
		t.Errorf("provisioned_iops moved off Current after a failed write:\n%s", body)
	}
}

func TestActuate_conflictNeverUpdatesManagedAndForcesRediscovery(t *testing.T) {
	// issue #16: a concurrent-modification conflict must never write
	// anything into a.managed (there is nothing verified to adopt), and
	// must force a fresh discover() rather than leave the stale snapshot
	// in place until the next scheduled DiscoveryInterval tick.
	client := &stubClient{vm: &compute.VirtualMachine{Name: "vm-1", VMType: "t1"}}
	conflictErr := fmt.Errorf("update independent disk %q: %w (expected current 1000, live 5000)", "vm-1-data", compute.ErrConcurrentModification)
	act := erroringActuator{err: conflictErr}
	a := newDiscoverableTestAgent(t, act, client)
	a.managed = []registry.ManagedDisk{independentDisk()}

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	// The stub VM has no disks at all, so a successful forced rediscovery
	// replaces the stale single-disk snapshot with an empty one — proof
	// that discover() actually ran and overwrote a.managed, not just that
	// GetVirtualMachine was called.
	if len(a.managed) != 0 {
		t.Errorf("managed = %+v, want empty — forced rediscovery must replace the stale snapshot with a fresh one, never leave the conflicting entry in place", a.managed)
	}
	if client.getVMCalls != 1 {
		t.Errorf("GetVirtualMachine called %d times, want 1 — a conflict must force rediscovery, not wait for the next scheduled cycle", client.getVMCalls)
	}
	if a.lastDiscoverySuccess.IsZero() {
		t.Error("lastDiscoverySuccess still zero after the forced rediscovery — it should have succeeded")
	}
}

func TestActuate_nonConflictErrorDoesNotForceRediscovery(t *testing.T) {
	client := &stubClient{vm: &compute.VirtualMachine{Name: "vm-1", VMType: "t1"}}
	act := erroringActuator{err: errors.New("some other transient failure")}
	a := newDiscoverableTestAgent(t, act, client)
	a.managed = []registry.ManagedDisk{independentDisk()}

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if client.getVMCalls != 0 {
		t.Errorf("GetVirtualMachine called %d times, want 0 — only a concurrent-modification conflict should force rediscovery", client.getVMCalls)
	}
}

// fixedDecider always returns the same Decision regardless of Analysis —
// used to prove reconcile's discovery-staleness gate (issue #15) blocks
// the whole pipeline before the Decider (and therefore the Actuator) is
// ever reached, not just that this cycle happened to decide NoAction.
type fixedDecider struct {
	action decider.Action
	target int64
}

func (f fixedDecider) Decide(diskID string, current int64, _ analyzer.Analysis, _ decider.VMState, _ decider.EffectivePolicy, _ decider.GuardrailState, _ time.Time) decider.Decision {
	return decider.Decision{DiskID: diskID, Action: f.action, Current: current, Target: f.target}
}

// emptySource is a metrics.MetricSource stub that always succeeds with no
// samples — reconcile's staleness gate must be checked before the sampler
// or Analyzer are even consulted, so what they'd return doesn't matter.
type emptySource struct{}

func (emptySource) Sample(_ context.Context, _ []metrics.DiskRef) ([]metrics.Sample, error) {
	return nil, nil
}
func (emptySource) Kind() metrics.SourceKind { return metrics.SourceGuest }

func newReconcileTestAgent(t *testing.T, lastDiscoverySuccess time.Time) (*Agent, *recordingActuator) {
	t.Helper()
	act := &recordingActuator{}
	budget, err := state.LoadBudget(t.TempDir() + "/budget.jsonl")
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	sampler, err := metrics.NewSampler(emptySource{}, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	return &Agent{
		vmName:               "vm-1",
		allowScaleDown:       true,
		maxDiscoveryAge:      time.Minute,
		lastDiscoverySuccess: lastDiscoverySuccess,
		// managedValid is a separate, complementary gate from the
		// age-based staleness these tests exercise (pre-merge review
		// finding) — true here so lastDiscoverySuccess's own age is what
		// actually decides the outcome, not this flag.
		managedValid: true,
		managed:      []registry.ManagedDisk{independentDisk()},
		sampler:      sampler,
		analyzer:     analyzer.NewWindowAnalyzer(),
		decider:      fixedDecider{action: decider.ActionScaleUp, target: 2000},
		actuator:     act,
		tracker:      state.NewTracker(budget),
		telemetry:    telemetry.NewRegistry(),
		journal:      journal.NewWriter(discard{}),
		readiness:    &health.Readiness{},
	}, act
}

func TestReconcile_skipsWhenDiscoveryStale(t *testing.T) {
	a, act := newReconcileTestAgent(t, time.Now().Add(-time.Hour)) // far past maxDiscoveryAge
	a.reconcile(context.Background())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — a stale discovery snapshot must never reach the Decider (issue #15)", len(act.calls))
	}
}

func TestReconcile_skipsWhenNeverDiscovered(t *testing.T) {
	a, act := newReconcileTestAgent(t, time.Time{}) // zero value: discover() never succeeded
	a.reconcile(context.Background())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — a zero-value lastDiscoverySuccess must fail closed, not be treated as fresh", len(act.calls))
	}
}

func TestReconcile_proceedsWhenDiscoveryFresh(t *testing.T) {
	a, act := newReconcileTestAgent(t, time.Now())
	a.reconcile(context.Background())
	if len(act.calls) != 1 {
		t.Errorf("Actuator.Apply called %d times, want 1 — a fresh discovery snapshot must not be blocked", len(act.calls))
	}
}

// TestReconcile_skipsImmediatelyAfterFatalDiscoveryError is the pre-merge
// review finding: a fatal discovery error must stop reconcile from acting
// on the previous managed set the instant it happens, not merely once
// maxDiscoveryAge eventually elapses. maxDiscoveryAge is set generously
// large here specifically so the age-based gate (issue #15, tested above)
// cannot be what blocks this cycle — only managedValid can.
func TestReconcile_skipsImmediatelyAfterFatalDiscoveryError(t *testing.T) {
	goodClient := &stubClient{
		vm: &compute.VirtualMachine{Name: "vm-1", VMType: "t1", Disks: []compute.StorageDisk{
			{Alias: "data", DeviceName: "datadisk", Class: compute.ClassIndependent, DiskName: "vm-1-data"},
		}},
		disks: map[string]*compute.Disk{"vm-1-data": {Name: "vm-1-data", SpecIOPS: 1000}},
	}
	act := &recordingActuator{}
	a := newDiscoverableTestAgent(t, act, goodClient)
	a.maxDiscoveryAge = time.Hour
	a.decider = fixedDecider{action: decider.ActionScaleUp, target: 2000}
	a.analyzer = analyzer.NewWindowAnalyzer()

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("first discover(): %v", err)
	}
	if !a.hasManagedDisks() {
		t.Fatal("test setup: first discover() did not populate the managed set")
	}

	// A second, failing discover() — the managed set from the first call
	// is still sitting in a.managed, well within maxDiscoveryAge.
	a.client = &stubClient{getVMErr: errors.New("simulated: transient Compute API failure")}
	if err := a.discover(context.Background()); err == nil {
		t.Fatal("test setup: second discover() unexpectedly succeeded")
	}

	a.reconcile(context.Background())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — a fatal discovery error must block reconcile immediately, not wait for maxDiscoveryAge", len(act.calls))
	}
}

// TestReconcile_skipsImmediatelyOnUnmatchedSelectedDisk is the same
// finding through the specific scenario the review named: an unmatched
// disks.selected device_name (issue #5 reopened), which fails inside
// registry.Select rather than compute.Discover.
func TestReconcile_skipsImmediatelyOnUnmatchedSelectedDisk(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(), // "good" and "bad", not "missing-disk"
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	act := &recordingActuator{}
	a := newDiscoverableTestAgent(t, act, client)
	a.maxDiscoveryAge = time.Hour
	a.decider = fixedDecider{action: decider.ActionScaleUp, target: 2000}
	a.analyzer = analyzer.NewWindowAnalyzer()
	a.cfg.Disks = config.DisksSpec{Mode: "auto-discovery"}

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("first discover(): %v", err)
	}

	a.cfg.Disks = config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "good"}, {DeviceName: "missing-disk"},
	}}
	if err := a.discover(context.Background()); err == nil {
		t.Fatal("test setup: second discover() with an unmatched selected disk unexpectedly succeeded")
	}

	a.reconcile(context.Background())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — an unmatched disks.selected device_name must block reconcile immediately", len(act.calls))
	}
}

// perDiskDecider returns a fixed Decision per DiskID, looked up by the
// caller — unlike fixedDecider, which returns the same action for every
// disk, this lets a reconcile test give different disks different
// verdicts in the same cycle (issue #19: vm_cap_reached must aggregate
// across disks, not reflect only whichever was decided last).
type perDiskDecider struct {
	byDisk map[string]decider.Action
}

func (p perDiskDecider) Decide(diskID string, current int64, _ analyzer.Analysis, _ decider.VMState, _ decider.EffectivePolicy, _ decider.GuardrailState, _ time.Time) decider.Decision {
	return decider.Decision{DiskID: diskID, Action: p.byDisk[diskID], Current: current}
}

// conflictOnFirstCallActuator returns a wrapped ErrConcurrentModification
// on its first Apply call and a plain success on every subsequent one —
// used to prove reconcile() aborts the rest of its cycle after the first
// disk's conflict rather than going on to actuate a second one.
type conflictOnFirstCallActuator struct {
	calls []decider.Decision
}

func (a *conflictOnFirstCallActuator) Apply(_ context.Context, d decider.Decision, _ actuator.Target, _ bool, _ string) (actuator.Applied, error) {
	a.calls = append(a.calls, d)
	if len(a.calls) == 1 {
		return actuator.Applied{}, fmt.Errorf("simulated: %w", compute.ErrConcurrentModification)
	}
	return actuator.Applied{DiskID: d.DiskID, IOPS: d.Target}, nil
}

// TestReconcile_abortsRestOfCycleAfterConcurrentModificationRediscovery is
// the pre-merge review finding: after a concurrent-modification conflict
// forces a rediscovery inside actuate(), the rest of this reconcile cycle
// — every disk not yet decided — was computed from a managed/iopsPool
// snapshot that rediscovery just superseded, and must not be acted on.
func TestReconcile_abortsRestOfCycleAfterConcurrentModificationRediscovery(t *testing.T) {
	client := &stubClient{
		vm: &compute.VirtualMachine{Name: "vm-1", VMType: "t1", Disks: []compute.StorageDisk{
			{Alias: "first", DeviceName: "first", Class: compute.ClassIndependent, DiskName: "vm-1-first"},
			{Alias: "second", DeviceName: "second", Class: compute.ClassIndependent, DiskName: "vm-1-second"},
		}},
		disks: map[string]*compute.Disk{
			"vm-1-first":  {Name: "vm-1-first", SpecIOPS: 1000},
			"vm-1-second": {Name: "vm-1-second", SpecIOPS: 1000},
		},
	}
	act := &conflictOnFirstCallActuator{}
	a := newDiscoverableTestAgent(t, act, client)
	a.maxDiscoveryAge = time.Hour
	a.analyzer = analyzer.NewWindowAnalyzer()
	a.decider = perDiskDecider{byDisk: map[string]decider.Action{
		"vm-1-first":  decider.ActionScaleUp,
		"vm-1-second": decider.ActionScaleUp,
	}}

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	a.reconcile(context.Background())

	if len(act.calls) != 1 {
		t.Errorf("Actuator.Apply called %d times, want 1 — the cycle must abort after the first disk's conflict-triggered rediscovery, never reaching the second disk", len(act.calls))
	}
}

// TestReconcile_vmCapReachedAggregatesRegardlessOfDiskOrder is issue #19's
// full-pipeline version of the telemetry-package aggregation test: two
// managed disks, only one of which is VM-cap-blocked, in both possible
// map/slice iteration orders — vm_cap_reached must read 1 either way, not
// depend on which disk reconcile happens to process last.
func TestReconcile_vmCapReachedAggregatesRegardlessOfDiskOrder(t *testing.T) {
	capped := registry.ManagedDisk{Disk: compute.StorageDisk{
		Alias: "capped", DeviceName: "capped", Class: compute.ClassIndependent, DiskName: "vm-1-capped",
	}}
	fine := registry.ManagedDisk{Disk: compute.StorageDisk{
		Alias: "fine", DeviceName: "fine", Class: compute.ClassIndependent, DiskName: "vm-1-fine",
	}}
	d := perDiskDecider{byDisk: map[string]decider.Action{
		"vm-1-capped": decider.ActionVMCapReached,
		"vm-1-fine":   decider.ActionNoAction,
	}}

	orders := [][]registry.ManagedDisk{
		{capped, fine},
		{fine, capped},
	}
	for i, order := range orders {
		t.Run(fmt.Sprintf("order %d", i), func(t *testing.T) {
			act := &recordingActuator{}
			budget, err := state.LoadBudget(t.TempDir() + "/budget.jsonl")
			if err != nil {
				t.Fatalf("LoadBudget: %v", err)
			}
			sampler, err := metrics.NewSampler(emptySource{}, 10)
			if err != nil {
				t.Fatalf("NewSampler: %v", err)
			}
			a := &Agent{
				vmName:               "vm-1",
				allowScaleDown:       true,
				maxDiscoveryAge:      time.Minute,
				lastDiscoverySuccess: time.Now(),
				managedValid:         true,
				managed:              order,
				sampler:              sampler,
				analyzer:             analyzer.NewWindowAnalyzer(),
				decider:              d,
				actuator:             act,
				tracker:              state.NewTracker(budget),
				telemetry:            telemetry.NewRegistry(),
				journal:              journal.NewWriter(discard{}),
				readiness:            &health.Readiness{},
			}
			a.reconcile(context.Background())

			body := scrapeAgentTelemetry(t, a)
			if !strings.Contains(body, `iops_autoscaler_vm_cap_reached{vm="vm-1"} 1`) {
				t.Errorf("vm_cap_reached not aggregated to 1 (order: %v):\n%s", order, body)
			}
		})
	}
}

// --- issue #4 (reopened): exclude disks that are not safely manageable ---

func vmWithTwoIndependentDisks() *compute.VirtualMachine {
	return &compute.VirtualMachine{
		Name:   "vm-1",
		VMType: "t1",
		Disks: []compute.StorageDisk{
			{Alias: "good", DeviceName: "good", Class: compute.ClassIndependent, DiskName: "vm-1-good"},
			{Alias: "bad", DeviceName: "bad", Class: compute.ClassIndependent, DiskName: "vm-1-bad"},
		},
	}
}

func TestDiscover_excludesDiskFailingGuestPathVerification(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	a.guestPathVerifier = func(ref metrics.DiskRef) error {
		if ref.DeviceName == "bad" {
			return errors.New("simulated: unresolvable guest path")
		}
		return nil
	}

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	if len(a.managed) != 1 || a.managed[0].Disk.DeviceName != "good" {
		t.Fatalf("managed = %+v, want only the disk that passed guest-path verification", a.managed)
	}
}

func TestDiscover_allDisksFailingVerificationLeavesEmptyManagedSet(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	a.guestPathVerifier = func(metrics.DiskRef) error {
		return errors.New("simulated: nothing verifies")
	}

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(a.managed) != 0 {
		t.Fatalf("managed = %+v, want empty", a.managed)
	}
	if a.hasManagedDisks() {
		t.Error("hasManagedDisks() = true, want false — readiness must not report a usable metrics path when nothing can produce a sample")
	}
}

func TestHasManagedDisks(t *testing.T) {
	a, _ := newTestAgent(t, true, &recordingActuator{})
	if a.hasManagedDisks() {
		t.Error("hasManagedDisks() = true on a freshly built Agent with no managed disks yet")
	}
	a.managed = []registry.ManagedDisk{independentDisk()}
	if !a.hasManagedDisks() {
		t.Error("hasManagedDisks() = false with a non-empty managed set")
	}
}

// --- issue #5 (reopened): unmatched disks.selected must be fatal ---

func TestDiscover_unmatchedSelectedDeviceIsFatalAndPreservesPriorManaged(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(), // has "good" and "bad", not "missing-disk"
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	a.cfg.Disks = config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "good"}, {DeviceName: "missing-disk"},
	}}
	// Simulate a prior successful cycle: every readiness dimension already
	// true, and a real managed set already published.
	a.readiness.SetConfigValid(true)
	a.readiness.SetDiscoverySucceeded(true)
	a.readiness.SetSourceReachable(true)
	stale := []registry.ManagedDisk{independentDisk()}
	a.managed = stale
	if !a.readiness.Ready() {
		t.Fatal("test setup: readiness must be true before the fatal discover() call")
	}

	err := a.discover(context.Background())
	if err == nil {
		t.Fatal("discover succeeded despite an unmatched disks.selected device_name")
	}
	if !strings.Contains(err.Error(), "missing-disk") {
		t.Errorf("err = %v, want it to name the unmatched device_name", err)
	}
	if len(a.managed) != 1 || a.managed[0].Disk.DeviceName != stale[0].Disk.DeviceName {
		t.Errorf("managed = %+v, want the prior snapshot left untouched, never replaced by a partial one", a.managed)
	}
	if a.readiness.Ready() {
		t.Error("readiness.Ready() = true after a fatal disks.selected mismatch, want false")
	}
}

func TestDiscover_selectedSucceedsWhenEveryDeviceMatches(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	a.cfg.Disks = config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{{DeviceName: "good"}}}

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(a.managed) != 1 || a.managed[0].Disk.DeviceName != "good" {
		t.Errorf("managed = %+v, want exactly the selected disk", a.managed)
	}
}

// --- issue #10 (reopened): crash-safe/fail-closed budget persistence ---

func TestActuate_reservesBeforeWriteAndConfirmsAfterSuccess(t *testing.T) {
	act := &recordingActuator{}
	a, tracker := newTestAgent(t, true, act)
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := tracker.State("vm-1-data", time.Now()).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — a successful write must be reserved and confirmed", got)
	}
	if len(tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none — a successful write must be Confirmed, not left Pending", tracker.PendingChanges())
	}
}

// --- issue: don't release the budget reservation on a bare actuator error
// — a real write's error does not prove nothing landed, so the reservation
// must be resolved from a fresh live read instead. ---

func TestActuate_writeFailure_liveStillAtBefore_releases(t *testing.T) {
	act := &erroringActuatorRecording{err: errors.New("simulated write failure")}
	a, tracker := newTestAgent(t, true, act)
	a.client = &stubClient{disks: map[string]*compute.Disk{
		"vm-1-data": {Name: "vm-1-data", SpecIOPS: 1000}, // live still matches Before
	}}
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if len(act.calls) != 1 {
		t.Fatalf("Actuator.Apply called %d times, want 1", len(act.calls))
	}
	if got := tracker.State("vm-1-data", time.Now()).ChangesInLast24h; got != 0 {
		t.Errorf("ChangesInLast24h = %d, want 0 — live state confirmed the write never landed, so it must be released", got)
	}
	if len(tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none after a released reservation", tracker.PendingChanges())
	}
}

func TestActuate_writeFailure_liveAtTarget_confirmsNotReleases(t *testing.T) {
	// The platform may have applied the change before the response reached
	// this process (timeout, connection reset). A bare error must not
	// release a reservation that actually landed — that would silently
	// undercount a real change against the 24h cap.
	act := &erroringActuatorRecording{err: errors.New("simulated: response lost after the request landed")}
	a, tracker := newTestAgent(t, true, act)
	a.managed = []registry.ManagedDisk{independentDisk()}
	a.client = &stubClient{disks: map[string]*compute.Disk{
		"vm-1-data": {Name: "vm-1-data", SpecIOPS: 2000}, // live already at Target
	}}
	now := time.Now()
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, now)

	if got := tracker.State("vm-1-data", now).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — a write confirmed landed via live state must still count", got)
	}
	if len(tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none — the reservation must be Confirmed, not left Pending", tracker.PendingChanges())
	}
	if got := tracker.State("vm-1-data", now).LastChangeUp; got.IsZero() {
		t.Error("cooldown was not started for a write confirmed landed via live state")
	}
	if got := a.managed[0].Disk.IOPS; got != 2000 {
		t.Errorf("managed disk IOPS = %d, want 2000 — the live value must be adopted even though Apply itself errored", got)
	}
}

func TestActuate_writeFailure_liveAmbiguous_staysPending(t *testing.T) {
	act := &erroringActuatorRecording{err: errors.New("simulated write failure")}
	a, tracker := newTestAgent(t, true, act)
	a.client = &stubClient{disks: map[string]*compute.Disk{
		"vm-1-data": {Name: "vm-1-data", SpecIOPS: 5000}, // matches neither Before nor Target
	}}
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := tracker.State("vm-1-data", time.Now()).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — an unexplained live value must stay pending (fail closed), not be released", got)
	}
	if len(tracker.PendingChanges()) != 1 {
		t.Errorf("PendingChanges() = %+v, want exactly one still-pending reservation", tracker.PendingChanges())
	}
}

func TestActuate_writeFailure_liveReadFails_staysPending(t *testing.T) {
	act := &erroringActuatorRecording{err: errors.New("simulated write failure")}
	a, tracker := newTestAgent(t, true, act)
	a.client = &stubClient{disks: map[string]*compute.Disk{}} // GetDisk fails: "stub: unused"
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := tracker.State("vm-1-data", time.Now()).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — a failed live-state read must leave the reservation pending, not release it", got)
	}
	if len(tracker.PendingChanges()) != 1 {
		t.Errorf("PendingChanges() = %+v, want exactly one still-pending reservation", tracker.PendingChanges())
	}
}

func TestActuate_dryRunErrorAlwaysReleases(t *testing.T) {
	// A dry run's error can never be ambiguous (no mutating call is ever
	// reached), so it must release unconditionally without needing — or
	// attempting — a live-state read.
	act := &erroringActuatorRecording{err: errors.New("simulated dry-run validation failure")}
	a, tracker := newTestAgent(t, true, act)
	a.dryRun = true
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := tracker.State("vm-1-data", time.Now()).ChangesInLast24h; got != 0 {
		t.Errorf("ChangesInLast24h = %d, want 0 — a dry-run error must always release, never stay pending", got)
	}
	if len(tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none", tracker.PendingChanges())
	}
}

func TestActuate_releasesReservationOnDryRun(t *testing.T) {
	act := &recordingActuator{}
	a, tracker := newTestAgent(t, true, act)
	a.dryRun = true
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := tracker.State("vm-1-data", time.Now()).ChangesInLast24h; got != 0 {
		t.Errorf("ChangesInLast24h = %d, want 0 — a dry run must never consume real budget", got)
	}
}

func TestActuate_refusesWriteWhenBudgetUnhealthy(t *testing.T) {
	act := &recordingActuator{}
	a := newTestAgentWithUnhealthyBudget(t, act)

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — an unhealthy budget must refuse before ever reaching the Actuator", len(act.calls))
	}
}

// newTestAgentWithUnhealthyBudget builds an Agent whose Tracker is already
// unhealthy — by pointing its Budget at a directory that's then removed,
// so one deliberate Reserve call fails and flips Healthy() false, the same
// mechanism a real filesystem/permissions failure would use in production.
// This can only be built this indirectly from outside internal/state: its
// fields (Budget.path, Tracker.budget) are unexported, and rightly so.
func newTestAgentWithUnhealthyBudget(t *testing.T, act actuator.Actuator) *Agent {
	t.Helper()
	sub := t.TempDir() + "/sub"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	budget, err := state.LoadBudget(sub + "/budget.jsonl")
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	tracker := state.NewTracker(budget)
	if err := os.RemoveAll(sub); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := tracker.ReserveChange("force-unhealthy", "force-unhealthy-key", 0, 0, time.Now()); err == nil {
		t.Fatal("test setup: expected ReserveChange to fail once the budget directory is gone")
	}
	if tracker.Healthy() {
		t.Fatal("test setup: expected the tracker to be unhealthy after a failed Reserve")
	}
	return &Agent{
		vmName:         "vm-1",
		allowScaleDown: true,
		actuator:       act,
		tracker:        tracker,
		telemetry:      telemetry.NewRegistry(),
		journal:        journal.NewWriter(discard{}),
	}
}

// erroringActuatorRecording is erroringActuator plus call recording, for
// tests that need both.
type erroringActuatorRecording struct {
	err   error
	calls []decider.Decision
}

func (e *erroringActuatorRecording) Apply(_ context.Context, d decider.Decision, _ actuator.Target, _ bool, _ string) (actuator.Applied, error) {
	e.calls = append(e.calls, d)
	return actuator.Applied{}, e.err
}

func TestReconcilePendingBudget_confirmsWhenLiveMatchesTarget(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 2000}, // matches Target below — the write landed
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if err := a.tracker.ReserveChange("vm-1-good", "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none — a reservation whose live current matches Target must be Confirmed", a.tracker.PendingChanges())
	}
	if got := a.tracker.State("vm-1-good", time.Now()).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — Confirm must still count", got)
	}
}

func TestReconcilePendingBudget_releasesWhenLiveMatchesBefore(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000}, // matches Before — the write never landed
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if err := a.tracker.ReserveChange("vm-1-good", "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none", a.tracker.PendingChanges())
	}
	if got := a.tracker.State("vm-1-good", time.Now()).ChangesInLast24h; got != 0 {
		t.Errorf("ChangesInLast24h = %d, want 0 — Release must stop it counting", got)
	}
}

func TestReconcilePendingBudget_ambiguousLiveValueStaysPending(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 5000}, // neither Before(1000) nor Target(2000)
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if err := a.tracker.ReserveChange("vm-1-good", "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 1 {
		t.Errorf("PendingChanges() = %+v, want the reservation left pending — an ambiguous live value must never be guessed at", a.tracker.PendingChanges())
	}
	if got := a.tracker.State("vm-1-good", time.Now()).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — still counted while unresolved", got)
	}
}

func TestReconcilePendingBudget_detachedDiskLeftPending(t *testing.T) {
	client := &stubClient{
		vm:    vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000}, "vm-1-bad": {Name: "vm-1-bad", SpecIOPS: 1000}},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	// Reserve for a disk that is not (and never was, in this test) part of
	// the managed set — simulating a disk detached between the crash and
	// this restart.
	if err := a.tracker.ReserveChange("vm-1-detached", "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 1 {
		t.Errorf("PendingChanges() = %+v, want the detached disk's reservation left pending — nothing to verify against, so it must not be silently dropped", a.tracker.PendingChanges())
	}
}
