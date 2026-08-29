package telemetry

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	r.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestRecordDecision_exposesExpectedSeries(t *testing.T) {
	r := NewRegistry()
	d := decider.Decision{
		DiskID:  "datadisk",
		Action:  decider.ActionScaleUp,
		Reason:  "saturated",
		Current: 1000,
		Target:  1200,
		Demand:  1100,
		Inputs: analyzer.Analysis{
			Source:              metrics.SourceGuest,
			IOPSUtilization:     0.98,
			ReadOps:             700,
			WriteOps:            300,
			Baseline:            analyzer.LatencyStat{P95: 2 * time.Millisecond},
			Saturated:           true,
			WindowFillUpRatio:   1.0,
			WindowFillDownRatio: 0.3,
		},
		VMHeadroomIOPS:  4500,
		BudgetRemaining: 47,
	}
	r.RecordDecision("vm-1", "independent", d)
	body := scrape(t, r)

	wantSubstrings := []string{
		`iops_autoscaler_disk_provisioned_iops{class="independent",disk="datadisk",vm="vm-1"} 1200`,
		`iops_autoscaler_disk_observed_iops{disk="datadisk",op="read",vm="vm-1"} 700`,
		`iops_autoscaler_disk_observed_iops{disk="datadisk",op="write",vm="vm-1"} 300`,
		`iops_autoscaler_disk_utilization_ratio{disk="datadisk",vm="vm-1"} 0.98`,
		`iops_autoscaler_disk_estimated_demand_iops{disk="datadisk",vm="vm-1"} 1100`,
		`iops_autoscaler_disk_latency_baseline_seconds{disk="datadisk",vm="vm-1"} 0.002`,
		`iops_autoscaler_disk_saturated{disk="datadisk",vm="vm-1"} 1`,
		`iops_autoscaler_decisions_total{action="scale_up",reason="saturated"} 1`,
		`iops_autoscaler_vm_headroom_iops{vm="vm-1"} 4500`,
		`iops_autoscaler_vm_cap_reached{vm="vm-1"} 0`,
		`iops_autoscaler_metric_source{disk="datadisk",source="guest"} 1`,
		`iops_autoscaler_window_fill_ratio{direction="up",disk="datadisk"} 1`,
		`iops_autoscaler_window_fill_ratio{direction="down",disk="datadisk"} 0.3`,
		`iops_autoscaler_change_budget_remaining{disk="datadisk"} 47`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("scraped output missing %q\n--- full output ---\n%s", want, body)
		}
	}
}

func TestRecordDecision_vmCapReachedSetsGauge(t *testing.T) {
	r := NewRegistry()
	d := decider.Decision{DiskID: "d1", Action: decider.ActionVMCapReached, Reason: "vm_headroom_insufficient"}
	r.RecordDecision("vm-1", "dependent", d)
	body := scrape(t, r)
	if !strings.Contains(body, `iops_autoscaler_vm_cap_reached{vm="vm-1"} 1`) {
		t.Errorf("vm_cap_reached not set to 1 on ActionVMCapReached:\n%s", body)
	}
}

func TestRecordDecision_decisionsTotalAccumulates(t *testing.T) {
	r := NewRegistry()
	d := decider.Decision{DiskID: "d1", Action: decider.ActionNoAction, Reason: "no_signal"}
	r.RecordDecision("vm-1", "independent", d)
	r.RecordDecision("vm-1", "independent", d)
	body := scrape(t, r)
	if !strings.Contains(body, `iops_autoscaler_decisions_total{action="no_action",reason="no_signal"} 2`) {
		t.Errorf("decisions_total did not accumulate to 2:\n%s", body)
	}
}

func TestRecordStaleness(t *testing.T) {
	r := NewRegistry()
	r.RecordStaleness("d1", 42.5)
	body := scrape(t, r)
	if !strings.Contains(body, `iops_autoscaler_metric_staleness_seconds{disk="d1"} 42.5`) {
		t.Errorf("metric_staleness_seconds not exposed as expected:\n%s", body)
	}
}
