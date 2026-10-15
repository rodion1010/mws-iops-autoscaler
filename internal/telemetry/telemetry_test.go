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
		// Decisions expose Current as provisioned state; Target remains intent.
		`iops_autoscaler_disk_provisioned_iops{class="independent",disk="datadisk",vm="vm-1"} 1000`,
		`iops_autoscaler_disk_proposed_target_iops{class="independent",disk="datadisk",vm="vm-1"} 1200`,
		`iops_autoscaler_disk_observed_iops{disk="datadisk",op="read",vm="vm-1"} 700`,
		`iops_autoscaler_disk_observed_iops{disk="datadisk",op="write",vm="vm-1"} 300`,
		`iops_autoscaler_disk_utilization_ratio{disk="datadisk",vm="vm-1"} 0.98`,
		`iops_autoscaler_disk_estimated_demand_iops{disk="datadisk",vm="vm-1"} 1100`,
		`iops_autoscaler_disk_latency_baseline_seconds{disk="datadisk",vm="vm-1"} 0.002`,
		`iops_autoscaler_disk_saturated{disk="datadisk",vm="vm-1"} 1`,
		`iops_autoscaler_decisions_total{action="scale_up",reason="saturated"} 1`,
		`iops_autoscaler_vm_headroom_iops{disk="datadisk",vm="vm-1"} 4500`,
		`iops_autoscaler_metric_source{disk="datadisk",source="guest"} 1`,
		`iops_autoscaler_metric_source{disk="datadisk",source="platform"} 0`,
		`iops_autoscaler_window_fill_ratio{direction="up",disk="datadisk"} 1`,
		`iops_autoscaler_window_fill_ratio{direction="down",disk="datadisk"} 0.3`,
		`iops_autoscaler_change_budget_remaining{disk="datadisk"} 47`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("scraped output missing %q\n--- full output ---\n%s", want, body)
		}
	}
	// RecordDecision must not create the VM-wide cap series.
	if strings.Contains(body, "iops_autoscaler_vm_cap_reached") {
		t.Error("vm_cap_reached must not be set by RecordDecision — only RecordVMCapReached sets it")
	}
}

func TestRecordDecision_neverMovesProvisionedOnDryRunOrFailedWrite(t *testing.T) {
	r := NewRegistry()
	d := decider.Decision{DiskID: "d1", Action: decider.ActionScaleUp, Current: 1000, Target: 5000}
	r.RecordDecision("vm-1", "independent", d)
	body := scrape(t, r)

	if !strings.Contains(body, `iops_autoscaler_disk_provisioned_iops{class="independent",disk="d1",vm="vm-1"} 1000`) {
		t.Errorf("provisioned_iops moved off Current despite no verified write:\n%s", body)
	}
	if !strings.Contains(body, `iops_autoscaler_disk_proposed_target_iops{class="independent",disk="d1",vm="vm-1"} 5000`) {
		t.Errorf("proposed_target_iops missing the decided target:\n%s", body)
	}
}

func TestRecordAppliedIOPS_movesProvisionedOnlyOnVerifiedWrite(t *testing.T) {
	r := NewRegistry()
	d := decider.Decision{DiskID: "d1", Action: decider.ActionScaleUp, Current: 1000, Target: 1200}
	r.RecordDecision("vm-1", "independent", d)
	r.RecordAppliedIOPS("d1", "vm-1", "independent", 1200)
	body := scrape(t, r)

	if !strings.Contains(body, `iops_autoscaler_disk_provisioned_iops{class="independent",disk="d1",vm="vm-1"} 1200`) {
		t.Errorf("provisioned_iops did not move to the verified applied value:\n%s", body)
	}
}

func TestRecordVMCapReached_aggregatesAcrossDisksRegardlessOfOrder(t *testing.T) {
	cases := []struct {
		name  string
		order []bool // per-disk: true = ActionVMCapReached
	}{
		{"capped disk processed first", []bool{true, false}},
		{"capped disk processed last", []bool{false, true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			any := false
			for i, capped := range tc.order {
				action := decider.ActionNoAction
				if capped {
					action = decider.ActionVMCapReached
					any = true
				}
				r.RecordDecision("vm-1", "independent", decider.Decision{DiskID: diskName(i), Action: action})
			}
			r.RecordVMCapReached("vm-1", any)
			body := scrape(t, r)
			if !strings.Contains(body, `iops_autoscaler_vm_cap_reached{vm="vm-1"} 1`) {
				t.Errorf("vm_cap_reached not aggregated to 1 regardless of which disk in the cycle capped (order: %v):\n%s", tc.order, body)
			}
		})
	}
}

func TestRecordVMCapReached_falseWhenNoDiskCapped(t *testing.T) {
	r := NewRegistry()
	r.RecordDecision("vm-1", "independent", decider.Decision{DiskID: "d1", Action: decider.ActionNoAction})
	r.RecordDecision("vm-1", "independent", decider.Decision{DiskID: "d2", Action: decider.ActionScaleUp})
	r.RecordVMCapReached("vm-1", false)
	body := scrape(t, r)
	if !strings.Contains(body, `iops_autoscaler_vm_cap_reached{vm="vm-1"} 0`) {
		t.Errorf("vm_cap_reached not 0 when no disk capped:\n%s", body)
	}
}

func TestRecordVMMinIOPSFloorExceedsPool(t *testing.T) {
	cases := []struct {
		name    string
		exceeds bool
		want    string
	}{
		{"floors sum past the pool", true, `iops_autoscaler_vm_min_iops_floor_exceeds_pool{vm="vm-1"} 1`},
		{"floors fit within the pool", false, `iops_autoscaler_vm_min_iops_floor_exceeds_pool{vm="vm-1"} 0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			r.RecordVMMinIOPSFloorExceedsPool("vm-1", tc.exceeds)
			body := scrape(t, r)
			if !strings.Contains(body, tc.want) {
				t.Errorf("body does not contain %q:\n%s", tc.want, body)
			}
		})
	}
}

func diskName(i int) string {
	if i == 0 {
		return "d1"
	}
	return "d2"
}

func TestRecordDecision_metricSourceClearsStaleActiveSeries(t *testing.T) {
	r := NewRegistry()
	r.RecordDecision("vm-1", "independent", decider.Decision{
		DiskID: "d1", Inputs: analyzer.Analysis{Source: metrics.SourceGuest},
	})
	body := scrape(t, r)
	if !strings.Contains(body, `iops_autoscaler_metric_source{disk="d1",source="guest"} 1`) {
		t.Fatalf("test setup: guest not active after first call:\n%s", body)
	}
	if !strings.Contains(body, `iops_autoscaler_metric_source{disk="d1",source="platform"} 0`) {
		t.Fatalf("test setup: platform not 0 after first call:\n%s", body)
	}

	r.RecordDecision("vm-1", "independent", decider.Decision{
		DiskID: "d1", Inputs: analyzer.Analysis{Source: metrics.SourcePlatform},
	})
	body = scrape(t, r)
	if !strings.Contains(body, `iops_autoscaler_metric_source{disk="d1",source="guest"} 0`) {
		t.Errorf("guest series must read back 0 after the active source switched to platform:\n%s", body)
	}
	if !strings.Contains(body, `iops_autoscaler_metric_source{disk="d1",source="platform"} 1`) {
		t.Errorf("platform series must read 1 as the newly active source:\n%s", body)
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

func TestSyncDiskConfiguredUnmanaged_setsCurrentEntries(t *testing.T) {
	r := NewRegistry()
	r.SyncDiskConfiguredUnmanaged(map[string]string{"datadisk": "selected_not_found"})
	body := scrape(t, r)
	if !strings.Contains(body, `iops_autoscaler_disk_configured_unmanaged{device_name="datadisk",reason="selected_not_found"} 1`) {
		t.Errorf("disk_configured_unmanaged not exposed as expected:\n%s", body)
	}
}

func TestSyncDiskConfiguredUnmanaged_deletesResolvedEntriesRatherThanZeroing(t *testing.T) {
	r := NewRegistry()
	r.SyncDiskConfiguredUnmanaged(map[string]string{"datadisk": "selected_not_found"})
	r.SyncDiskConfiguredUnmanaged(map[string]string{}) // datadisk resolved this cycle

	body := scrape(t, r)
	if strings.Contains(body, "iops_autoscaler_disk_configured_unmanaged") {
		t.Errorf("disk_configured_unmanaged still exposed after the disk resolved, want the series deleted entirely (not set to 0):\n%s", body)
	}
}

func TestSyncDiskConfiguredUnmanaged_onlyDeletesEntriesNoLongerCurrent(t *testing.T) {
	r := NewRegistry()
	r.SyncDiskConfiguredUnmanaged(map[string]string{
		"datadisk": "selected_not_found",
		"logdisk":  "selected_not_found",
	})
	r.SyncDiskConfiguredUnmanaged(map[string]string{"logdisk": "selected_not_found"}) // datadisk resolved

	body := scrape(t, r)
	if strings.Contains(body, `device_name="datadisk"`) {
		t.Errorf("datadisk still exposed after resolving:\n%s", body)
	}
	if !strings.Contains(body, `iops_autoscaler_disk_configured_unmanaged{device_name="logdisk",reason="selected_not_found"} 1`) {
		t.Errorf("logdisk missing from disk_configured_unmanaged, want it still exposed:\n%s", body)
	}
}
