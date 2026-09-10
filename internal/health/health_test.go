package health

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzAlwaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestReadyz_notReadyByDefault(t *testing.T) {
	r := &Readiness{}
	rec := httptest.NewRecorder()
	r.ReadyzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503 for a freshly started process", rec.Code)
	}
	var body readyzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Ready {
		t.Error("body.Ready = true, want false")
	}
}

func TestReadyz_readyOnlyWhenAllThreeConditionsHold(t *testing.T) {
	r := &Readiness{}
	r.SetConfigValid(true)
	r.SetDiscoverySucceeded(true)

	if r.Ready() {
		t.Fatal("Ready() = true with only 2 of 3 conditions set")
	}

	rec := httptest.NewRecorder()
	r.ReadyzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503 (source not reachable)", rec.Code)
	}

	r.SetSourceReachable(true)
	if !r.Ready() {
		t.Fatal("Ready() = false with all 3 conditions set")
	}

	rec = httptest.NewRecorder()
	r.ReadyzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200 once all conditions hold", rec.Code)
	}
	var body readyzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if !body.Ready || !body.ConfigValid || !body.DiscoverySucceeded || !body.SourceReachable {
		t.Errorf("body = %+v, want every field true", body)
	}
}

func TestReadyz_canFlipBackToNotReady(t *testing.T) {
	r := &Readiness{}
	r.SetConfigValid(true)
	r.SetDiscoverySucceeded(true)
	r.SetSourceReachable(true)
	if !r.Ready() {
		t.Fatal("test setup: expected Ready() = true")
	}

	r.SetSourceReachable(false) // e.g. the metrics source went unreachable
	if r.Ready() {
		t.Error("Ready() = true after SourceReachable flipped back to false")
	}
}

func TestReadyz_configuredDisksUnmanagedOmittedWhenEmpty(t *testing.T) {
	r := &Readiness{}
	r.SetConfigValid(true)
	r.SetDiscoverySucceeded(true)
	r.SetSourceReachable(true)

	rec := httptest.NewRecorder()
	r.ReadyzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if strings.Contains(rec.Body.String(), "configured_disks_unmanaged") {
		t.Errorf("body = %s, want no configured_disks_unmanaged key when nothing is degraded — the healthy-state response must stay byte-for-byte what it was before this field existed", rec.Body.String())
	}
}

func TestReadyz_configuredDisksUnmanagedReportedButNeverFlipsReady(t *testing.T) {
	r := &Readiness{}
	r.SetConfigValid(true)
	r.SetDiscoverySucceeded(true)
	r.SetSourceReachable(true)
	r.SetDegradedDisks([]DegradedDisk{{DeviceName: "datadisk", Reason: "selected_not_found"}})

	if !r.Ready() {
		t.Fatal("Ready() = false with a degraded disk set — /readyz's binary verdict must never fold in degradation (docs/design.md §4.1)")
	}

	rec := httptest.NewRecorder()
	r.ReadyzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200 — degradation alone must not make /readyz unready", rec.Code)
	}
	var body readyzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if len(body.ConfiguredDisksUnmanaged) != 1 || body.ConfiguredDisksUnmanaged[0].DeviceName != "datadisk" {
		t.Errorf("body.ConfiguredDisksUnmanaged = %+v, want [{datadisk selected_not_found}]", body.ConfiguredDisksUnmanaged)
	}

	// A disk that resolves again must disappear from the body entirely, not
	// linger as a stale entry.
	r.SetDegradedDisks(nil)
	rec = httptest.NewRecorder()
	r.ReadyzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if strings.Contains(rec.Body.String(), "configured_disks_unmanaged") {
		t.Errorf("body = %s, want configured_disks_unmanaged gone once SetDegradedDisks(nil) clears it", rec.Body.String())
	}
}
