package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/actuator"
	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/health"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
	"github.com/rodion1010/mws-iops-autoscaler/internal/registry"
)

// Model a same-name replacement with unchanged IOPS, which an IOPS-only check cannot distinguish.
type replacementClient struct {
	disk *compute.Disk
	vm   *compute.VirtualMachine

	independentWrites int
	dependentWrites   int
}

func (c *replacementClient) GetVirtualMachine(_ context.Context, _ string) (*compute.VirtualMachine, error) {
	if c.vm == nil {
		return nil, errors.New("replacement client: VM not configured")
	}
	cp := *c.vm
	cp.Disks = append([]compute.StorageDisk(nil), c.vm.Disks...)
	return &cp, nil
}

func (c *replacementClient) GetVmType(context.Context, string) (*compute.VmType, error) {
	return &compute.VmType{VCPUCount: 2}, nil
}

func (c *replacementClient) GetDisk(_ context.Context, _ string) (*compute.Disk, error) {
	if c.disk == nil {
		return nil, errors.New("replacement client: disk not configured")
	}
	cp := *c.disk
	return &cp, nil
}

func (c *replacementClient) UpdateIndependentDisk(_ context.Context, req compute.IndependentUpdateRequest) (*compute.Disk, error) {
	c.independentWrites++
	c.disk.SpecIOPS = req.IOPS
	cp := *c.disk
	return &cp, nil
}

func (c *replacementClient) UpdateDependentDisk(_ context.Context, req compute.DependentUpdateRequest) (*compute.VirtualMachine, error) {
	c.dependentWrites++
	for i := range c.vm.Disks {
		if c.vm.Disks[i].Alias == req.TargetAlias {
			c.vm.Disks[i].IOPS = req.TargetIOPS
		}
	}
	cp := *c.vm
	cp.Disks = append([]compute.StorageDisk(nil), c.vm.Disks...)
	return &cp, nil
}

func TestRegressionStaleDecisionNeverMutatesReplacementDisk(t *testing.T) {
	t.Run("independent disk", func(t *testing.T) {
		client := &replacementClient{disk: &compute.Disk{
			Name: "vm-1-data", SpecIOPS: 1000, Etag: "etag-replacement", IncarnationID: "incarnation-b",
		}}
		m := independentDisk()
		m.Disk.IOPS = 1000
		m.Disk.IncarnationID = "incarnation-a"

		a := newDiscoverableTestAgent(t, actuator.NewComputeActuator(client), client)
		a.managed = []registry.ManagedDisk{m}
		d := decider.Decision{DiskID: m.Disk.DiskName, Action: decider.ActionScaleUp, Current: 1000, Target: 2000}

		a.actuate(context.Background(), m, d, time.Now())

		if client.independentWrites != 0 {
			t.Fatalf("replacement disk received %d write(s), want 0: the decision belongs to a different incarnation", client.independentWrites)
		}
		if client.disk.SpecIOPS != 1000 {
			t.Fatalf("replacement disk IOPS = %d, want unchanged 1000", client.disk.SpecIOPS)
		}
	})

	t.Run("dependent disk", func(t *testing.T) {
		client := &replacementClient{
			vm: &compute.VirtualMachine{
				Name: "vm-1", Etag: "etag-replacement",
				Disks: []compute.StorageDisk{{
					Alias: "bootdisk", DeviceName: "bootdisk", DiskName: "vm-1-boot",
					Class: compute.ClassDependent, IOPS: 1000,
					// Real VM reads omit IncarnationID; only the fresh GetDisk below can verify identity.
				}},
			},
			// The fresh disk read returns the replacement incarnation.
			disk: &compute.Disk{Name: "vm-1-boot", SpecIOPS: 1000, IncarnationID: "incarnation-b"},
		}
		m := dependentDisk()
		m.Disk.IOPS = 1000
		m.Disk.IncarnationID = "incarnation-a"

		a := newDiscoverableTestAgent(t, actuator.NewComputeActuator(client), client)
		a.managed = []registry.ManagedDisk{m}
		d := decider.Decision{DiskID: m.Disk.DiskName, Action: decider.ActionScaleUp, Current: 1000, Target: 2000}

		a.actuate(context.Background(), m, d, time.Now())

		if client.dependentWrites != 0 {
			t.Fatalf("replacement attachment received %d write(s), want 0: the decision belongs to a different incarnation", client.dependentWrites)
		}
		if client.vm.Disks[0].IOPS != 1000 {
			t.Fatalf("replacement attachment IOPS = %d, want unchanged 1000", client.vm.Disks[0].IOPS)
		}
	})
}

func TestRegressionPendingReservationNeverResolvesAgainstReplacementDisk(t *testing.T) {
	client := &replacementClient{disk: &compute.Disk{
		Name: "vm-1-data", SpecIOPS: 2000, Etag: "etag-replacement", IncarnationID: "incarnation-b",
	}}
	m := independentDisk()
	m.Disk.IOPS = 1000
	m.Disk.IncarnationID = "incarnation-a"

	a, tracker := newTestAgent(t, &recordingActuator{})
	a.client = client
	a.managed = []registry.ManagedDisk{m}
	if err := tracker.ReserveChange(m.Disk.IdentityKey(), "reservation-a", 1000, 2000, time.Now()); err != nil {
		t.Fatalf("ReserveChange: %v", err)
	}

	a.reconcilePendingBudget(context.Background())

	if got := len(tracker.PendingChanges()); got != 1 {
		t.Fatalf("pending reservations = %d, want 1: incarnation-b cannot prove whether incarnation-a's write landed", got)
	}
}

func TestRegressionConflictRediscoveryFailureAbortsCycle(t *testing.T) {
	client := &stubClient{
		vm: vmWithTwoIndependentDisks(),
		disks: map[string]*compute.Disk{
			"vm-1-good": {Name: "vm-1-good", SpecIOPS: 1000},
			"vm-1-bad":  {Name: "vm-1-bad", SpecIOPS: 1000},
		},
	}
	act := &conflictOnFirstCallActuator{}
	a := newDiscoverableTestAgent(t, act, client)
	a.maxDiscoveryAge = time.Hour
	a.analyzer = analyzer.NewWindowAnalyzer()
	a.decider = perDiskDecider{byDisk: map[string]decider.Action{
		"vm-1-good": decider.ActionScaleUp,
		"vm-1-bad":  decider.ActionScaleUp,
	}}
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("initial discover: %v", err)
	}
	client.getVMErr = errors.New("rediscovery unavailable")

	a.reconcile(context.Background())

	if len(act.calls) != 1 {
		t.Fatalf("actuator calls = %d, want 1: a failed forced rediscovery invalidates the snapshot and must abort the cycle", len(act.calls))
	}
}

func TestRegressionPreWriteConflictDoesNotConsumeChangeBudget(t *testing.T) {
	act := &erroringActuatorRecording{err: fmt.Errorf("pre-write refusal: %w", compute.ErrConcurrentModification)}
	a, tracker := newTestAgent(t, act)
	a.readiness = &health.Readiness{}
	a.client = &stubClient{
		getVMErr: errors.New("rediscovery unavailable"),
		disks: map[string]*compute.Disk{
			"vm-1-data": {Name: "vm-1-data", SpecIOPS: 5000},
		},
	}
	d := decider.Decision{DiskID: "vm-1-data", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}

	a.actuate(context.Background(), independentDisk(), d, time.Now())

	if got := len(tracker.PendingChanges()); got != 0 {
		t.Fatalf("pending reservations = %d, want 0: ErrConcurrentModification proves the mutating request was never sent", got)
	}
}

type fixedSamplesSource struct {
	samples []metrics.Sample
}

func (s fixedSamplesSource) Sample(context.Context, []metrics.DiskRef) ([]metrics.Sample, error) {
	return append([]metrics.Sample(nil), s.samples...), nil
}

func (fixedSamplesSource) Kind() metrics.SourceKind { return metrics.SourceGuest }

type analysisCapturingDecider struct {
	analyses []analyzer.Analysis
}

func (d *analysisCapturingDecider) Decide(diskID string, current int64, a analyzer.Analysis, _ decider.VMState, _ decider.EffectivePolicy, _ decider.GuardrailState, _ time.Time) decider.Decision {
	d.analyses = append(d.analyses, a)
	return decider.Decision{DiskID: diskID, Action: decider.ActionNoAction, Current: current}
}

func latencySamples(diskID string, start time.Time, latency time.Duration) []metrics.Sample {
	samples := make([]metrics.Sample, 6)
	for i := range samples {
		samples[i] = metrics.Sample{
			DiskID:     diskID,
			Timestamp:  start.Add(time.Duration(i) * time.Minute),
			ReadOps:    100,
			ReadLatAvg: latency,
			QueueDepth: metrics.QueueDepth{Unit: metrics.QueueDepthOps},
			Source:     metrics.SourceGuest,
		}
	}
	return samples
}

func TestRegressionRecreatedDiskStartsWithFreshLatencyBaseline(t *testing.T) {
	now := time.Now()
	start := now.Add(-5 * time.Minute)
	windowAnalyzer := analyzer.NewWindowAnalyzer()
	policy := analyzer.EvaluationPolicy{
		ScaleUpWindow:             5 * time.Minute,
		ScaleDownWindow:           5 * time.Minute,
		UtilizationThreshold:      0.95,
		QueueDepthThreshold:       1,
		LatencyBaselineMultiplier: 1.5,
		MaxSampleAge:              time.Minute,
		MaxSampleGap:              90 * time.Second,
	}
	original := independentDisk()
	original.Disk.IncarnationID = "incarnation-a"
	seed := windowAnalyzer.Analyze(original.Disk.DiskName, latencySamples(original.Disk.DiskName, start, time.Millisecond), policy, 1000, now)
	if !seed.BaselineEstablished || seed.Baseline.P95 != time.Millisecond {
		t.Fatalf("test setup: original baseline = %+v, established = %v", seed.Baseline, seed.BaselineEstablished)
	}

	recreated := independentDisk()
	recreated.Disk.IncarnationID = "incarnation-b"
	recreated.Disk.IOPS = 1000
	ref := metrics.DiskRef{
		DiskID: recreated.Disk.DiskName, VMName: "vm-1", DeviceName: recreated.Disk.DeviceName, IncarnationID: recreated.Disk.IncarnationID,
	}
	sampler, err := metrics.NewSampler(fixedSamplesSource{samples: latencySamples(recreated.Disk.DiskName, start, 10*time.Millisecond)}, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	sampler.SetDisks([]metrics.DiskRef{ref})
	if err := sampler.Tick(context.Background()); err != nil {
		t.Fatalf("sampler Tick: %v", err)
	}

	a, _, _ := newReconcileTestAgent(t, now)
	capture := &analysisCapturingDecider{}
	a.managed = []registry.ManagedDisk{recreated}
	a.sampler = sampler
	a.analyzer = windowAnalyzer
	a.decider = capture
	a.maxSampleAge = time.Minute
	a.maxSampleGap = 90 * time.Second
	a.maxDiscoveryAge = time.Hour
	a.lastDiscoverySuccess = now
	a.managedValid = true

	a.reconcile(context.Background())

	if len(capture.analyses) != 1 {
		t.Fatalf("captured analyses = %d, want 1", len(capture.analyses))
	}
	got := capture.analyses[0]
	if !got.BaselineEstablished {
		t.Fatal("replacement disk did not establish its own baseline")
	}
	if got.Baseline.P95 != 10*time.Millisecond {
		t.Fatalf("replacement baseline = %s, want 10ms: it must not inherit incarnation-a's 1ms baseline", got.Baseline.P95)
	}
	// Analyzer state uses the composite identity key, but exported DiskID must remain human-readable.
	if got.DiskID != recreated.Disk.DiskName {
		t.Errorf("Analysis.DiskID = %q, want the plain disk name %q, not the analyzer's internal composite state key",
			got.DiskID, recreated.Disk.DiskName)
	}
}
