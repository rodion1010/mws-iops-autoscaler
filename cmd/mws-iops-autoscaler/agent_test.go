package main

import (
	"context"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/actuator"
	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
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
