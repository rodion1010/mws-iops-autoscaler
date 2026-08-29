package main

import (
	"context"
	"errors"
	"fmt"
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
}

func (s *stubClient) GetVirtualMachine(_ context.Context, _ string) (*compute.VirtualMachine, error) {
	s.getVMCalls++
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
		managed:              []registry.ManagedDisk{independentDisk()},
		sampler:              sampler,
		analyzer:             analyzer.NewWindowAnalyzer(),
		decider:              fixedDecider{action: decider.ActionScaleUp, target: 2000},
		actuator:             act,
		tracker:              state.NewTracker(budget),
		telemetry:            telemetry.NewRegistry(),
		journal:              journal.NewWriter(discard{}),
		readiness:            &health.Readiness{},
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

func TestActuate_releasesReservationOnWriteFailure(t *testing.T) {
	act := &erroringActuatorRecording{err: errors.New("simulated write failure")}
	a, tracker := newTestAgent(t, true, act)
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if len(act.calls) != 1 {
		t.Fatalf("Actuator.Apply called %d times, want 1", len(act.calls))
	}
	if got := tracker.State("vm-1-data", time.Now()).ChangesInLast24h; got != 0 {
		t.Errorf("ChangesInLast24h = %d, want 0 — a failed write's reservation must be released, not left counted", got)
	}
	if len(tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none after a released reservation", tracker.PendingChanges())
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
