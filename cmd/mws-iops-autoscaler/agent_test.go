package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func scrapeAgentTelemetry(t *testing.T, a *Agent) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	a.telemetry.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func testResolvedPolicy() config.ResolvedPolicy {
	return config.ResolvedPolicy{
		MinIOPS: 1000,
		MaxIOPS: 10000,
		Advanced: config.ResolvedAdvanced{
			ScaleUpWindow:             5 * time.Minute,
			ScaleDownWindow:           30 * time.Minute,
			UtilizationThreshold:      0.95,
			QueueDepthThreshold:       1,
			LatencyMode:               "relative",
			LatencyBaselineMultiplier: 1.5,
			ScaleUpUtilization:        0.8,
			ScaleDownUtilization:      0.4,
			CooldownScaleUp:           5 * time.Minute,
			CooldownScaleDown:         30 * time.Minute,
			MinChangeIOPS:             100,
			MaxScaleUpRatio:           2.0,
			MaxScaleDownRatio:         0.5,
			MaxChangesPer24h:          4,
		},
	}
}

func dependentDisk() registry.ManagedDisk {
	return registry.ManagedDisk{
		Disk: compute.StorageDisk{
			Alias: "bootdisk", DeviceName: "bootdisk", Class: compute.ClassDependent, DiskName: "vm-1-boot",
		},
		Policy: testResolvedPolicy(),
	}
}

func independentDisk() registry.ManagedDisk {
	return registry.ManagedDisk{
		Disk: compute.StorageDisk{
			Alias: "datadisk", DeviceName: "datadisk", Class: compute.ClassIndependent, DiskName: "vm-1-data",
		},
		Policy: testResolvedPolicy(),
	}
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
	if got := tracker.State(independentDisk().Disk.IdentityKey(), time.Now()).LastChangeUp; got.IsZero() {
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

type stubClient struct {
	getVMCalls int
	vm         *compute.VirtualMachine
	disks      map[string]*compute.Disk
	getVMErr   error
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

type erroringActuator struct{ err error }

func (e erroringActuator) Apply(context.Context, decider.Decision, actuator.Target, bool, string) (actuator.Applied, error) {
	return actuator.Applied{}, e.err
}

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
	// A successful write must immediately update the managed snapshot.
	act := &recordingActuator{}
	a, _ := newTestAgent(t, true, act)
	a.managed = []registry.ManagedDisk{independentDisk()}

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := a.managed[0].Disk.IOPS; got != 2000 {
		t.Errorf("managed disk IOPS after a successful write = %d, want 2000 (the applied value, adopted immediately)", got)
	}
}

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
	// A stale decision must not write or adopt the externally changed value.
	client := &stubClient{vm: &compute.VirtualMachine{Name: "vm-1", VMType: "t1"}}
	conflictErr := fmt.Errorf("update independent disk %q: %w (expected current 1000, live 5000)", "vm-1-data", compute.ErrConcurrentModification)
	act := erroringActuator{err: conflictErr}
	a := newDiscoverableTestAgent(t, act, client)
	a.managed = []registry.ManagedDisk{independentDisk()}

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	// Empty rediscovery invalidates the previous managed snapshot.
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

type fixedDecider struct {
	action decider.Action
	target int64
}

func (f fixedDecider) Decide(diskID string, current int64, _ analyzer.Analysis, _ decider.VMState, _ decider.EffectivePolicy, _ decider.GuardrailState, _ time.Time) decider.Decision {
	return decider.Decision{DiskID: diskID, Action: f.action, Current: current, Target: f.target}
}

type emptySource struct{}

func (emptySource) Sample(_ context.Context, _ []metrics.DiskRef) ([]metrics.Sample, error) {
	return nil, nil
}
func (emptySource) Kind() metrics.SourceKind { return metrics.SourceGuest }

func newReconcileTestAgent(t *testing.T, lastDiscoverySuccess time.Time) (*Agent, *recordingActuator, *bytes.Buffer) {
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
	var journalOut bytes.Buffer
	return &Agent{
		vmName:               "vm-1",
		allowScaleDown:       true,
		maxDiscoveryAge:      time.Minute,
		lastDiscoverySuccess: lastDiscoverySuccess,
		// Immediate invalidation and age-based staleness are independent guards.
		managedValid: true,
		managed:      []registry.ManagedDisk{independentDisk()},
		sampler:      sampler,
		analyzer:     analyzer.NewWindowAnalyzer(),
		decider:      fixedDecider{action: decider.ActionScaleUp, target: 2000},
		actuator:     act,
		tracker:      state.NewTracker(budget),
		telemetry:    telemetry.NewRegistry(),
		journal:      journal.NewWriter(&journalOut),
		readiness:    &health.Readiness{},
	}, act, &journalOut
}

func TestReconcile_skipsWhenDiscoveryStale(t *testing.T) {
	a, act, journalOut := newReconcileTestAgent(t, time.Now().Add(-time.Hour)) // far past maxDiscoveryAge
	a.reconcile(context.Background())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — a stale discovery snapshot must never reach the Decider (issue #15)", len(act.calls))
	}
	assertSingleJournalReason(t, journalOut, "discovery_stale")
}

func TestReconcile_skipsWhenNeverDiscovered(t *testing.T) {
	a, act, journalOut := newReconcileTestAgent(t, time.Time{}) // zero value: discover() never succeeded
	a.reconcile(context.Background())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — a zero-value lastDiscoverySuccess must fail closed, not be treated as fresh", len(act.calls))
	}
	assertSingleJournalReason(t, journalOut, "discovery_stale")
}

func TestReconcile_proceedsWhenDiscoveryFresh(t *testing.T) {
	a, act, journalOut := newReconcileTestAgent(t, time.Now())
	a.reconcile(context.Background())
	if len(act.calls) != 1 {
		t.Errorf("Actuator.Apply called %d times, want 1 — a fresh discovery snapshot must not be blocked", len(act.calls))
	}
	if journalOut.Len() == 0 {
		t.Error("journal is empty on a normal reconcile — every cycle must emit a record, discovery_stale or otherwise (design.md §4.3)")
	}
	if strings.Contains(journalOut.String(), `"discovery_stale"`) {
		t.Error("journal contains a discovery_stale record on a fresh-discovery cycle that reached the Decider")
	}
}

func TestReconcile_doesNotPublishStalenessForColdStartDisk(t *testing.T) {
	a, _, _ := newReconcileTestAgent(t, time.Now())
	a.reconcile(context.Background())

	body := scrapeAgentTelemetry(t, a)
	if strings.Contains(body, "iops_autoscaler_metric_staleness_seconds") {
		t.Errorf("metric_staleness_seconds published for a disk that has never had a sample:\n%s", body)
	}
}

func assertSingleJournalReason(t *testing.T, journalOut *bytes.Buffer, want string) {
	t.Helper()
	assertJournalReasons(t, journalOut, 1, want)
}

func assertJournalReasons(t *testing.T, journalOut *bytes.Buffer, wantCount int, wantReason string) {
	t.Helper()
	trimmed := strings.TrimSpace(journalOut.String())
	var lines []string
	if trimmed != "" {
		lines = strings.Split(trimmed, "\n")
	}
	if len(lines) != wantCount {
		t.Fatalf("journal has %d lines, want %d: %q", len(lines), wantCount, journalOut.String())
	}
	for _, line := range lines {
		var rec journal.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal journal line: %v", err)
		}
		if rec.Reason != wantReason {
			t.Errorf("journal record reason = %q, want %q", rec.Reason, wantReason)
		}
		if rec.Action != string(decider.ActionNoAction) {
			t.Errorf("journal record action = %q, want %q", rec.Action, decider.ActionNoAction)
		}
	}
}

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
	var journalOut bytes.Buffer
	a.journal = journal.NewWriter(&journalOut)

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("first discover(): %v", err)
	}
	if !a.hasManagedDisks() {
		t.Fatal("test setup: first discover() did not populate the managed set")
	}

	// Fail discovery after a previously successful snapshot.
	a.client = &stubClient{getVMErr: errors.New("simulated: transient Compute API failure")}
	if err := a.discover(context.Background()); err == nil {
		t.Fatal("test setup: second discover() unexpectedly succeeded")
	}

	a.reconcile(context.Background())
	if len(act.calls) != 0 {
		t.Errorf("Actuator.Apply called %d times, want 0 — a fatal discovery error must block reconcile immediately, not wait for maxDiscoveryAge", len(act.calls))
	}
	assertSingleJournalReason(t, &journalOut, "discovery_stale")
}

// vmWithGoodAndMissingDisk and vmWithGoodDiskOnly model the same VM, one
// discovery cycle apart, after "missing-disk" detaches — the config never
// changes between the two calls, only the VM's live inventory does. That
// is what makes the second discover() call here a genuine *runtime*
// mismatch (docs/design.md §4.1) rather than a cold start: a real process
// never mutates cfg.Disks mid-run (config hot-reload is out of scope,
// docs/TODO.md), so a test simulating the mismatch must vary the VM
// instead — mutating cfg.Disks between two discover() calls, as an earlier
// version of this test did, cannot happen outside a test and made the test
// pass for the wrong reason.
func vmWithGoodAndMissingDisk() *compute.VirtualMachine {
	return &compute.VirtualMachine{Name: "vm-1", VMType: "t1", Disks: []compute.StorageDisk{
		{Alias: "good", DeviceName: "good", Class: compute.ClassIndependent, DiskName: "vm-1-good"},
		{Alias: "missing-disk", DeviceName: "missing-disk", Class: compute.ClassIndependent, DiskName: "vm-1-missing"},
	}}
}

func vmWithGoodDiskOnly() *compute.VirtualMachine {
	return &compute.VirtualMachine{Name: "vm-1", VMType: "t1", Disks: []compute.StorageDisk{
		{Alias: "good", DeviceName: "good", Class: compute.ClassIndependent, DiskName: "vm-1-good"},
	}}
}

func TestReconcile_degradesPartiallyWhenASelectedDiskIsRemovedAtRuntime(t *testing.T) {
	firstClient := &stubClient{
		vm: vmWithGoodAndMissingDisk(),
		disks: map[string]*compute.Disk{
			"vm-1-good":    {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-missing": {Name: "vm-1-missing", SpecIOPS: 1000},
		},
	}
	act := &recordingActuator{}
	a := newDiscoverableTestAgent(t, act, firstClient)
	a.maxDiscoveryAge = time.Hour
	a.decider = fixedDecider{action: decider.ActionScaleUp, target: 2000}
	a.analyzer = analyzer.NewWindowAnalyzer()
	a.cfg.Disks = config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "good"}, {DeviceName: "missing-disk"},
	}}

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("first discover(): %v", err)
	}
	if len(a.managed) != 2 {
		t.Fatalf("test setup: first discover() managed = %+v, want both disks", a.managed)
	}

	// missing-disk detaches — same config, a later cycle.
	a.client = &stubClient{
		vm:    vmWithGoodDiskOnly(),
		disks: map[string]*compute.Disk{"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000}},
	}
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("second discover() after missing-disk detached at runtime: %v, want no error — a runtime mismatch degrades, it does not block discovery", err)
	}
	if len(a.managed) != 1 || a.managed[0].Disk.DeviceName != "good" {
		t.Errorf("managed = %+v, want exactly [good] — the rest of the set must keep being managed", a.managed)
	}
	if len(a.degraded) != 1 || a.degraded[0].DeviceName != "missing-disk" || a.degraded[0].Reason != registry.ReasonSelectedNotFound {
		t.Errorf("degraded = %+v, want exactly [{missing-disk %s}]", a.degraded, registry.ReasonSelectedNotFound)
	}

	// Degradation must never flip the binary verdict (docs/design.md §4.1).
	// discover() doesn't own config_valid/source_reachable; set them
	// directly so Ready() isn't false for an unrelated reason.
	a.readiness.SetConfigValid(true)
	a.readiness.SetSourceReachable(true)
	if !a.readiness.Ready() {
		t.Error("Ready() = false after a runtime-only degradation, want true")
	}
	if body := scrapeAgentTelemetry(t, a); !strings.Contains(body, `iops_autoscaler_disk_configured_unmanaged{device_name="missing-disk",reason="selected_not_found"} 1`) {
		t.Errorf("disk_configured_unmanaged not exposed as expected:\n%s", body)
	}

	a.reconcile(context.Background())
	if len(act.calls) != 1 || act.calls[0].DiskID != "vm-1-good" {
		t.Errorf("Actuator.Apply calls = %+v, want exactly one call for vm-1-good — the resolved disk must keep being managed despite the other's degradation", act.calls)
	}

	// missing-disk reattaches — degradation must clear, not linger.
	a.client = &stubClient{
		vm: vmWithGoodAndMissingDisk(),
		disks: map[string]*compute.Disk{
			"vm-1-good":    {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-missing": {Name: "vm-1-missing", SpecIOPS: 1000},
		},
	}
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("third discover() after missing-disk reattached: %v", err)
	}
	if len(a.degraded) != 0 {
		t.Errorf("degraded = %+v after reattaching, want none", a.degraded)
	}
	if body := scrapeAgentTelemetry(t, a); strings.Contains(body, "iops_autoscaler_disk_configured_unmanaged") {
		t.Errorf("disk_configured_unmanaged still exposed after missing-disk reattached, want the series deleted:\n%s", body)
	}
}

type perDiskDecider struct {
	byDisk map[string]decider.Action
}

func (p perDiskDecider) Decide(diskID string, current int64, _ analyzer.Analysis, _ decider.VMState, _ decider.EffectivePolicy, _ decider.GuardrailState, _ time.Time) decider.Decision {
	return decider.Decision{DiskID: diskID, Action: p.byDisk[diskID], Current: current}
}

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

type incarnationMismatchOnFirstCallActuator struct {
	calls []decider.Decision
}

func (a *incarnationMismatchOnFirstCallActuator) Apply(_ context.Context, d decider.Decision, _ actuator.Target, _ bool, _ string) (actuator.Applied, error) {
	a.calls = append(a.calls, d)
	if len(a.calls) == 1 {
		return actuator.Applied{}, fmt.Errorf("simulated: %w", compute.ErrIncarnationMismatch)
	}
	return actuator.Applied{DiskID: d.DiskID, IOPS: d.Target}, nil
}

func TestReconcile_abortsRestOfCycleAfterIncarnationMismatchRediscovery(t *testing.T) {
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
	act := &incarnationMismatchOnFirstCallActuator{}
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
		t.Errorf("Actuator.Apply called %d times, want 1 — the cycle must abort after the first disk's incarnation-mismatch-triggered rediscovery, never reaching the second disk", len(act.calls))
	}
}

func TestActuate_incarnationMismatchReleasesReservationAndForcesRediscovery(t *testing.T) {
	conflictErr := fmt.Errorf("update independent disk %q: %w (expected incarnation \"incarnation-a\", live \"incarnation-b\")",
		"vm-1-data", compute.ErrIncarnationMismatch)
	client := &stubClient{vm: &compute.VirtualMachine{Name: "vm-1", VMType: "t1"}}
	act := erroringActuator{err: conflictErr}
	a := newDiscoverableTestAgent(t, act, client)
	a.managed = []registry.ManagedDisk{independentDisk()}

	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := len(a.tracker.PendingChanges()); got != 0 {
		t.Errorf("PendingChanges() = %d, want 0 — an incarnation mismatch proves nothing was written, so the reservation must be released", got)
	}
	// Empty rediscovery invalidates the previous managed snapshot.
	if len(a.managed) != 0 {
		t.Errorf("managed = %+v, want empty — an incarnation mismatch must force rediscovery, not leave the stale snapshot in place", a.managed)
	}
	if client.getVMCalls != 1 {
		t.Errorf("GetVirtualMachine called %d times, want 1 — an incarnation mismatch must force rediscovery, not wait for the next scheduled cycle", client.getVMCalls)
	}
}

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

func TestDiscover_populatesUnmanagedFromDisksOutsideSelection(t *testing.T) {
	client := &stubClient{
		vm: &compute.VirtualMachine{
			Name: "vm-1", VMType: "t1",
			Disks: []compute.StorageDisk{
				{Alias: "data", DeviceName: "data", Class: compute.ClassIndependent, DiskName: "vm-1-data"},
				{Alias: "churn", DeviceName: "churn", Class: compute.ClassIndependent, DiskName: "vm-1-churn"},
				{Alias: "noname", DeviceName: "noname", Class: compute.ClassIndependent, DiskName: "vm-1-noname"},
			},
		},
		disks: map[string]*compute.Disk{
			"vm-1-data":   {Name: "vm-1-data", SpecIOPS: 1000},
			"vm-1-churn":  {Name: "vm-1-churn", SpecIOPS: 1000},
			"vm-1-noname": {Name: "vm-1-noname", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	a.cfg = &config.Config{Disks: config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{{DeviceName: "data"}}}}
	a.resolved = &config.ResolvedConfig{Disks: a.cfg.Disks, VMPolicy: testResolvedPolicy()}

	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	if len(a.managed) != 1 || a.managed[0].Disk.DeviceName != "data" {
		t.Fatalf("managed = %+v, want exactly [data]", a.managed)
	}
	gotNames := make(map[string]bool, len(a.unmanaged))
	for _, d := range a.unmanaged {
		gotNames[d.DeviceName] = true
	}
	// Before this fix, discover() never retained anything about a disk it
	// did not select — reconcile() had no way to know churn/noname existed
	// at all, let alone how much IOPS they were consuming (D4/US-009).
	if len(a.unmanaged) != 2 || !gotNames["churn"] || !gotNames["noname"] {
		t.Errorf("unmanaged = %+v, want exactly [churn, noname] — every VM disk outside the managed set (docs/design.md §4.6)", a.unmanaged)
	}
}

// fixedOpsSource returns a constant ops/s reading per DiskID on every Tick,
// enough for analyzer.demandP95Down to compute a real p95 — demandP95Down
// runs on whatever the trailing down-window actually holds and does not
// itself require WindowFullDown (analyzer.go), so a handful of ticks well
// under a production-scale ScaleDownWindow is sufficient here.
type fixedOpsSource struct {
	opsByDiskID map[string]float64
}

func (s fixedOpsSource) Sample(_ context.Context, disks []metrics.DiskRef) ([]metrics.Sample, error) {
	now := time.Now()
	var out []metrics.Sample
	for _, d := range disks {
		ops, ok := s.opsByDiskID[d.DiskID]
		if !ok {
			continue
		}
		out = append(out, metrics.Sample{DiskID: d.DiskID, Timestamp: now, ReadOps: ops, Source: metrics.SourceGuest})
	}
	return out, nil
}
func (fixedOpsSource) Kind() metrics.SourceKind { return metrics.SourceGuest }

// TestReconcile_vmHeadroomCountsUnmanagedSiblingConsumption is D4/US-009's
// regression test: docs/design.md §4.6 defines VM headroom from "other
// disks", not "other managed disks", but until this fix reconcile() only
// ever analyzed the managed set — an unmanaged sibling doing real, sustained
// I/O was silently treated as if it consumed zero of the shared pool.
func TestReconcile_vmHeadroomCountsUnmanagedSiblingConsumption(t *testing.T) {
	dataDisk := registry.ManagedDisk{
		Disk:   compute.StorageDisk{Alias: "data", DeviceName: "data", Class: compute.ClassIndependent, DiskName: "vm-1-data", IOPS: 1000},
		Policy: testResolvedPolicy(),
	}
	churnDisk := compute.StorageDisk{Alias: "churn", DeviceName: "churn", Class: compute.ClassIndependent, DiskName: "vm-1-churn", IOPS: 1000}

	source := fixedOpsSource{opsByDiskID: map[string]float64{
		"vm-1-data":  0,   // idle managed disk
		"vm-1-churn": 900, // heavily loaded, unmanaged sibling
	}}
	sampler, err := metrics.NewSampler(source, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	sampler.SetDisks([]metrics.DiskRef{{DiskID: "vm-1-data"}, {DiskID: "vm-1-churn"}})
	for i := 0; i < 3; i++ {
		if err := sampler.Tick(context.Background()); err != nil {
			t.Fatalf("sampler.Tick: %v", err)
		}
	}

	budget, err := state.LoadBudget(t.TempDir() + "/budget.jsonl")
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	var journalOut bytes.Buffer
	a := &Agent{
		vmName:               "vm-1",
		allowScaleDown:       true,
		maxDiscoveryAge:      time.Minute,
		lastDiscoverySuccess: time.Now(),
		managedValid:         true,
		managed:              []registry.ManagedDisk{dataDisk},
		unmanaged:            []compute.StorageDisk{churnDisk},
		iopsPool:             6000,
		resolved:             &config.ResolvedConfig{VMPolicy: testResolvedPolicy()},
		sampler:              sampler,
		analyzer:             analyzer.NewWindowAnalyzer(),
		decider:              decider.NewPolicyDecider(true),
		actuator:             &recordingActuator{},
		tracker:              state.NewTracker(budget),
		telemetry:            telemetry.NewRegistry(),
		journal:              journal.NewWriter(&journalOut),
		readiness:            &health.Readiness{},
	}
	a.reconcile(context.Background())

	lines := strings.Split(strings.TrimSpace(journalOut.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("journal has %d line(s), want exactly 1 (one managed disk): %s", len(lines), journalOut.String())
	}
	var rec struct {
		Disk           string `json:"disk"`
		VMHeadroomIOPS int64  `json:"vm_headroom_iops"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decode journal record: %v", err)
	}
	if rec.Disk != "vm-1-data" {
		t.Fatalf("journal record is for %q, want vm-1-data", rec.Disk)
	}
	// Before this fix, siblingP95 only ever summed the managed set (one
	// disk here, itself excluded), so vm_headroom_iops always read the full
	// 6000 pool no matter how loaded churn was.
	if want := int64(6000 - 900); rec.VMHeadroomIOPS != want {
		t.Errorf("vm_headroom_iops = %d, want %d (6000 pool - 900 unmanaged sibling p95, docs/design.md §4.6)", rec.VMHeadroomIOPS, want)
	}
}

// presetSource returns a fixed, caller-built sample set regardless of the
// disks requested — used to place samples at exact, controlled timestamps
// (real sources stamp Sample.Timestamp from time.Now(), which cannot build
// a deterministic multi-sample window fast enough for a unit test).
type presetSource struct {
	samples []metrics.Sample
}

func (p presetSource) Sample(_ context.Context, _ []metrics.DiskRef) ([]metrics.Sample, error) {
	return p.samples, nil
}
func (presetSource) Kind() metrics.SourceKind { return metrics.SourceGuest }

// buildSaturatedSamples returns 5 samples spanning 10 minutes ending at
// base, each pegged at specIOPS (utilization 1.0) with queueDepths applied
// in order — sustained utilization plus a growing queue satisfies both
// analyzer.Analysis.Saturated and its open-workload demand regime.
func buildSaturatedSamples(diskID string, base time.Time, specIOPS float64, queueDepths [5]float64) []metrics.Sample {
	out := make([]metrics.Sample, 5)
	for i := 0; i < 5; i++ {
		age := time.Duration(4-i) * 150 * time.Second // 2.5m steps, oldest first
		out[i] = metrics.Sample{
			DiskID:     diskID,
			Timestamp:  base.Add(-age),
			ReadOps:    specIOPS,
			QueueDepth: metrics.QueueDepth{Value: queueDepths[i], Unit: metrics.QueueDepthOps},
			Source:     metrics.SourceGuest,
		}
	}
	return out
}

// TestReconcile_unmeasurableSiblingBlocksScaleUp asserts docs/design.md
// §4.6 headroom must not substitute zero for a sibling's unmeasured demand.
// Blocking lands on ActionNoAction, not ActionVMCapReached, so a measurement
// gap never aliases onto the "add vCPU" signal (design.md §7.3).
func TestReconcile_unmeasurableSiblingBlocksScaleUp(t *testing.T) {
	dataDisk := registry.ManagedDisk{
		Disk:   compute.StorageDisk{Alias: "data", DeviceName: "data", Class: compute.ClassIndependent, DiskName: "vm-1-data", IOPS: 1000},
		Policy: testResolvedPolicy(),
	}
	churnDisk := compute.StorageDisk{Alias: "churn", DeviceName: "churn", Class: compute.ClassIndependent, DiskName: "vm-1-churn", IOPS: 1000}

	base := time.Now()
	dataSamples := buildSaturatedSamples("vm-1-data", base, 1000, [5]float64{1, 6, 11, 16, 21})

	tests := []struct {
		name         string
		churnSamples []metrics.Sample
		wantAction   decider.Action
		wantReason   string
	}{
		{
			name:         "fresh measurable sibling allows scale-up",
			churnSamples: buildSaturatedSamples("vm-1-churn", base, 1000, [5]float64{5, 5, 5, 5, 5}),
			wantAction:   decider.ActionScaleUp,
		},
		{
			name:         "missing sibling sample blocks scale-up",
			churnSamples: nil,
			wantAction:   decider.ActionNoAction,
			wantReason:   "sibling_demand_unmeasurable",
		},
		{
			name:         "stale sibling sample blocks scale-up",
			churnSamples: buildSaturatedSamples("vm-1-churn", base.Add(-2*time.Hour), 1000, [5]float64{5, 5, 5, 5, 5}),
			wantAction:   decider.ActionNoAction,
			wantReason:   "sibling_demand_unmeasurable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := presetSource{samples: append(append([]metrics.Sample(nil), dataSamples...), tc.churnSamples...)}
			sampler, err := metrics.NewSampler(source, 10)
			if err != nil {
				t.Fatalf("NewSampler: %v", err)
			}
			sampler.SetDisks([]metrics.DiskRef{{DiskID: "vm-1-data"}, {DiskID: "vm-1-churn"}})
			if err := sampler.Tick(context.Background()); err != nil {
				t.Fatalf("sampler.Tick: %v", err)
			}

			budget, err := state.LoadBudget(t.TempDir() + "/budget.jsonl")
			if err != nil {
				t.Fatalf("LoadBudget: %v", err)
			}
			var journalOut bytes.Buffer
			a := &Agent{
				vmName:               "vm-1",
				allowScaleDown:       true,
				maxDiscoveryAge:      time.Minute,
				maxSampleAge:         time.Hour,
				maxSampleGap:         10 * time.Minute,
				lastDiscoverySuccess: time.Now(),
				managedValid:         true,
				managed:              []registry.ManagedDisk{dataDisk},
				unmanaged:            []compute.StorageDisk{churnDisk},
				iopsPool:             6000,
				resolved:             &config.ResolvedConfig{VMPolicy: testResolvedPolicy()},
				sampler:              sampler,
				analyzer:             analyzer.NewWindowAnalyzer(),
				decider:              decider.NewPolicyDecider(true),
				actuator:             &recordingActuator{},
				tracker:              state.NewTracker(budget),
				telemetry:            telemetry.NewRegistry(),
				journal:              journal.NewWriter(&journalOut),
				readiness:            &health.Readiness{},
			}
			a.reconcile(context.Background())

			lines := strings.Split(strings.TrimSpace(journalOut.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("journal has %d line(s), want exactly 1 (one managed disk): %s", len(lines), journalOut.String())
			}
			var rec struct {
				Action string `json:"action"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
				t.Fatalf("decode journal record: %v", err)
			}
			if rec.Action != string(tc.wantAction) {
				t.Errorf("action = %q, want %q (journal: %s)", rec.Action, tc.wantAction, lines[0])
			}
			if tc.wantReason != "" && rec.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", rec.Reason, tc.wantReason)
			}
		})
	}
}

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

// TestReconcile_unresolvedGuestPathSiblingBlocksScaleUp covers the same
// docs/design.md §4.6 invariant as the test above, but drives a sibling
// into the unmeasured state via a real discover() call (unresolved guest
// path) instead of a hand-built sample set.
func TestReconcile_unresolvedGuestPathSiblingBlocksScaleUp(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	a.resolved = &config.ResolvedConfig{VMPolicy: testResolvedPolicy()}
	a.maxDiscoveryAge = time.Minute
	a.maxSampleAge = time.Hour
	a.maxSampleGap = 10 * time.Minute
	a.analyzer = analyzer.NewWindowAnalyzer()
	a.decider = decider.NewPolicyDecider(true)
	a.guestPathVerifier = func(ref metrics.DiskRef) error {
		if ref.DeviceName == "bad" {
			return errors.New("simulated: unresolvable guest path")
		}
		return nil
	}
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(a.managed) != 1 || a.managed[0].Disk.DiskName != "vm-1-good" {
		t.Fatalf("managed = %+v, want only vm-1-good", a.managed)
	}
	if len(a.unmanaged) != 1 || a.unmanaged[0].DiskName != "vm-1-bad" {
		t.Fatalf("unmanaged = %+v, want exactly [vm-1-bad]", a.unmanaged)
	}

	base := time.Now()
	dataSamples := buildSaturatedSamples("vm-1-good", base, 1000, [5]float64{1, 6, 11, 16, 21})
	source := presetSource{samples: dataSamples} // no samples at all for vm-1-bad
	sampler, err := metrics.NewSampler(source, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	sampler.SetDisks([]metrics.DiskRef{a.diskRef(a.managed[0]), a.diskRefForDisk(a.unmanaged[0])})
	if err := sampler.Tick(context.Background()); err != nil {
		t.Fatalf("sampler.Tick: %v", err)
	}
	a.sampler = sampler

	var journalOut bytes.Buffer
	a.journal = journal.NewWriter(&journalOut)
	a.reconcile(context.Background())

	lines := strings.Split(strings.TrimSpace(journalOut.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("journal has %d line(s), want exactly 1: %s", len(lines), journalOut.String())
	}
	var rec struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decode journal record: %v", err)
	}
	if rec.Action != string(decider.ActionNoAction) {
		t.Errorf("action = %q, want %q (not ActionVMCapReached — this is a measurement gap, not a real VM cap)", rec.Action, decider.ActionNoAction)
	}
	if rec.Reason != "sibling_demand_unmeasurable" {
		t.Errorf("reason = %q, want sibling_demand_unmeasurable", rec.Reason)
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

func TestDiscover_unmatchedSelectedDeviceAtColdStartIsFatal(t *testing.T) {
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
	if !a.lastDiscoverySuccess.IsZero() {
		t.Fatal("test setup: lastDiscoverySuccess must be zero — this test is specifically the cold-start case (docs/design.md §4.1)")
	}
	// Start from a previously healthy readiness state, and a managed set
	// from... somewhere else entirely (this agent never discovered
	// anything) — proving invalidateManaged leaves whatever was there
	// untouched is the point, not that it's realistic cold-start state.
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
		t.Fatal("discover succeeded despite an unmatched disks.selected device_name on the first discovery attempt")
	}
	if !strings.Contains(err.Error(), "missing-disk") {
		t.Errorf("err = %v, want it to name the unmatched device_name", err)
	}
	if len(a.managed) != 1 || a.managed[0].Disk.DeviceName != stale[0].Disk.DeviceName {
		t.Errorf("managed = %+v, want the prior snapshot left untouched, never replaced by a partial one", a.managed)
	}
	if a.readiness.Ready() {
		t.Error("readiness.Ready() = true after a fatal cold-start disks.selected mismatch, want false")
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

func TestActuate_reservesBeforeWriteAndConfirmsAfterSuccess(t *testing.T) {
	act := &recordingActuator{}
	a, tracker := newTestAgent(t, true, act)
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := tracker.State(independentDisk().Disk.IdentityKey(), time.Now()).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — a successful write must be reserved and confirmed", got)
	}
	if len(tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none — a successful write must be Confirmed, not left Pending", tracker.PendingChanges())
	}
}

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
	if got := tracker.State(independentDisk().Disk.IdentityKey(), time.Now()).ChangesInLast24h; got != 0 {
		t.Errorf("ChangesInLast24h = %d, want 0 — live state confirmed the write never landed, so it must be released", got)
	}
	if len(tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none after a released reservation", tracker.PendingChanges())
	}
}

func TestActuate_writeFailure_liveAtTarget_confirmsNotReleases(t *testing.T) {
	// The platform may have applied the request despite the returned transport error.
	act := &erroringActuatorRecording{err: errors.New("simulated: response lost after the request landed")}
	a, tracker := newTestAgent(t, true, act)
	a.managed = []registry.ManagedDisk{independentDisk()}
	a.client = &stubClient{disks: map[string]*compute.Disk{
		"vm-1-data": {Name: "vm-1-data", SpecIOPS: 2000}, // live already at Target
	}}
	now := time.Now()
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, now)

	if got := tracker.State(independentDisk().Disk.IdentityKey(), now).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — a write confirmed landed via live state must still count", got)
	}
	if len(tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none — the reservation must be Confirmed, not left Pending", tracker.PendingChanges())
	}
	if got := tracker.State(independentDisk().Disk.IdentityKey(), now).LastChangeUp; got.IsZero() {
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

	if got := tracker.State(independentDisk().Disk.IdentityKey(), time.Now()).ChangesInLast24h; got != 1 {
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

	if got := tracker.State(independentDisk().Disk.IdentityKey(), time.Now()).ChangesInLast24h; got != 1 {
		t.Errorf("ChangesInLast24h = %d, want 1 — a failed live-state read must leave the reservation pending, not release it", got)
	}
	if len(tracker.PendingChanges()) != 1 {
		t.Errorf("PendingChanges() = %+v, want exactly one still-pending reservation", tracker.PendingChanges())
	}
}

func TestActuate_dryRunErrorAlwaysReleases(t *testing.T) {
	// Dry-run errors cannot hide a mutation, so the reservation is releasable.
	act := &erroringActuatorRecording{err: errors.New("simulated dry-run validation failure")}
	a, tracker := newTestAgent(t, true, act)
	a.dryRun = true
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := tracker.State(independentDisk().Disk.IdentityKey(), time.Now()).ChangesInLast24h; got != 0 {
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

	if got := tracker.State(independentDisk().Disk.IdentityKey(), time.Now()).ChangesInLast24h; got != 0 {
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
	if err := a.tracker.ReserveChange(compute.StorageDisk{DiskName: "vm-1-good"}.IdentityKey(), "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none — a reservation whose live current matches Target must be Confirmed", a.tracker.PendingChanges())
	}
	if got := a.tracker.State(compute.StorageDisk{DiskName: "vm-1-good"}.IdentityKey(), time.Now()).ChangesInLast24h; got != 1 {
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
	if err := a.tracker.ReserveChange(compute.StorageDisk{DiskName: "vm-1-good"}.IdentityKey(), "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 0 {
		t.Errorf("PendingChanges() = %+v, want none", a.tracker.PendingChanges())
	}
	if got := a.tracker.State(compute.StorageDisk{DiskName: "vm-1-good"}.IdentityKey(), time.Now()).ChangesInLast24h; got != 0 {
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
	if err := a.tracker.ReserveChange(compute.StorageDisk{DiskName: "vm-1-good"}.IdentityKey(), "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 1 {
		t.Errorf("PendingChanges() = %+v, want the reservation left pending — an ambiguous live value must never be guessed at", a.tracker.PendingChanges())
	}
	if got := a.tracker.State(compute.StorageDisk{DiskName: "vm-1-good"}.IdentityKey(), time.Now()).ChangesInLast24h; got != 1 {
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
	// The reservation targets a disk absent from the managed set.
	if err := a.tracker.ReserveChange("vm-1-detached", "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 1 {
		t.Errorf("PendingChanges() = %+v, want the detached disk's reservation left pending — nothing to verify against, so it must not be silently dropped", a.tracker.PendingChanges())
	}
}

func TestReconcilePendingBudget_recreatedDiskUnderSameNameNeverMatchesStaleReservation(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000, IncarnationID: "incarnation-1"},
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	a := newDiscoverableTestAgent(t, &recordingActuator{}, client)
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	// The reservation belongs to the original incarnation.
	original := compute.StorageDisk{DiskName: "vm-1-good", IncarnationID: "incarnation-1"}
	if err := a.tracker.ReserveChange(original.IdentityKey(), "key-1", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	// Discovery now contains a replacement with the same name.
	client.disks["vm-1-good"] = &compute.Disk{Name: "vm-1-good", SpecIOPS: 2000, IncarnationID: "incarnation-2"}
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("second discover: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if len(a.tracker.PendingChanges()) != 1 {
		t.Errorf("PendingChanges() = %+v, want the stale reservation left pending — the new disk's live IOPS must never resolve the old incarnation's reservation", a.tracker.PendingChanges())
	}
}
