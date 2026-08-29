package health

import (
	"encoding/json"
	"net/http/httptest"
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
	// SourceReachable still false.

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
