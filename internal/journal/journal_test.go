package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

var ts = time.Date(2026, 8, 29, 15, 4, 5, 0, time.UTC)

func TestNewRecord_fieldMapping(t *testing.T) {
	d := decider.Decision{
		DiskID:  "db-data",
		Action:  decider.ActionNoAction,
		Reason:  "saturation_gate_not_met",
		Current: 5000,
		Target:  5000,
		Demand:  0,
		Inputs: analyzer.Analysis{
			IOPSUtilization:   0.62,
			QueueDepth:        metrics.QueueDepth{Value: 0, Unit: metrics.QueueDepthOps},
			ReadLatency:       analyzer.LatencyStat{P95: 1200 * time.Microsecond},
			WriteLatency:      analyzer.LatencyStat{P95: 900 * time.Microsecond},
			Baseline:          analyzer.LatencyStat{P95: 1100 * time.Microsecond},
			WindowFillUpRatio: 1.0,
			Source:            metrics.SourceGuest,
		},
		VMHeadroomIOPS: 4200,
	}
	got := NewRecord(ts, "database-01", "dependent", d)

	want := Record{
		TS: ts, Disk: "db-data", VM: "database-01", Class: "dependent",
		Action: "no_action", Reason: "saturation_gate_not_met",
		CurrentIOPS: 5000, TargetIOPS: 5000, DemandIOPS: 0,
		Utilization:       0.62,
		QueueDepth:        QueueDepth{Value: 0, Unit: "ops"},
		P95ReadLatencyMS:  1.2,
		P95WriteLatencyMS: 0.9,
		LatencyBaselineMS: 1.1,
		VMHeadroomIOPS:    4200,
		WindowFillUp:      1.0,
		Source:            "guest",
	}
	if got != want {
		t.Errorf("NewRecord() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestWriter_appendsOneJSONObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	r1 := NewRecord(ts, "vm-1", "independent", decider.Decision{DiskID: "d1", Action: decider.ActionScaleUp})
	r2 := NewRecord(ts, "vm-1", "independent", decider.Decision{DiskID: "d2", Action: decider.ActionNoAction})

	if err := w.Write(r1); err != nil {
		t.Fatalf("Write(r1): %v", err)
	}
	if err := w.Write(r2); err != nil {
		t.Fatalf("Write(r2): %v", err)
	}

	scanner := bufio.NewScanner(&buf)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (one JSON object per Write call)", len(lines))
	}
	var decoded Record
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("line 1 is not valid JSON: %v", err)
	}
	if decoded.Disk != "d1" {
		t.Errorf("line 1 Disk = %q, want d1", decoded.Disk)
	}
	if err := json.Unmarshal([]byte(lines[1]), &decoded); err != nil {
		t.Fatalf("line 2 is not valid JSON: %v", err)
	}
	if decoded.Disk != "d2" {
		t.Errorf("line 2 Disk = %q, want d2", decoded.Disk)
	}
}
