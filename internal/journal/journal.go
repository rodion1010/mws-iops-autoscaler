// Package journal writes the structured, one-record-per-disk-per-cycle
// decision log, from which "why didn't it scale?" must always be
// answerable alone. Every reconcile emits one Record, including NoAction;
// this package only shapes and writes that record, it never decides anything.
package journal

import (
	"encoding/json"
	"io"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
)

// QueueDepth mirrors metrics.QueueDepth's JSON shape — duplicated rather
// than imported so a journal consumer never needs internal/metrics to
// parse a record; the journal is an external contract, not an internal one.
type QueueDepth struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// Record is one reconcile cycle's decision for one disk. Its JSON field
// names are a stable external contract.
type Record struct {
	TS     time.Time `json:"ts"`
	Disk   string    `json:"disk"`
	VM     string    `json:"vm"`
	Class  string    `json:"class"` // dependent | independent
	Action string    `json:"action"`
	Reason string    `json:"reason"`

	CurrentIOPS int64   `json:"current_iops"`
	TargetIOPS  int64   `json:"target_iops"`
	DemandIOPS  float64 `json:"demand_iops"`

	Utilization       float64    `json:"utilization"`
	QueueDepth        QueueDepth `json:"queue_depth"`
	P95ReadLatencyMS  float64    `json:"p95_read_latency_ms"`
	P95WriteLatencyMS float64    `json:"p95_write_latency_ms"`
	LatencyBaselineMS float64    `json:"latency_baseline_ms"`

	VMHeadroomIOPS  int64   `json:"vm_headroom_iops"`
	BudgetRemaining int     `json:"budget_remaining"`
	WindowFillUp    float64 `json:"window_fill_up"`
	WindowFillDown  float64 `json:"window_fill_down"`

	// IOPSDemandDown/BandwidthDemandDownIOPSEquiv/LimitingResourceDown are
	// the down-window demand breakdown — DemandIOPS above is
	// whichever one of these two actually drove the target (or a
	// scale-up demand figure entirely, for a ScaleUp record); these three
	// exist so a ScaleDown/Underutilized record's journal entry can show
	// *why* — which resource was binding — not just the final number.
	IOPSDemandDown               float64 `json:"iops_demand_down"`
	BandwidthDemandDownIOPSEquiv float64 `json:"bandwidth_demand_down_iops_equiv"`
	LimitingResourceDown         string  `json:"limiting_resource_down"` // "iops" | "bandwidth"

	Source string `json:"source"` // guest | platform
}

// NewRecord builds a Record from one reconcile cycle's Decision. now, vmName
// and class come from the caller: Decide only takes what it needs to
// decide, not metadata a journal consumer wants.
func NewRecord(now time.Time, vmName, class string, d decider.Decision) Record {
	a := d.Inputs
	return Record{
		TS:     now,
		Disk:   d.DiskID,
		VM:     vmName,
		Class:  class,
		Action: string(d.Action),
		Reason: d.Reason,

		CurrentIOPS: d.Current,
		TargetIOPS:  d.Target,
		DemandIOPS:  d.Demand,

		Utilization:       a.IOPSUtilization,
		QueueDepth:        QueueDepth{Value: a.QueueDepth.Value, Unit: string(a.QueueDepth.Unit)},
		P95ReadLatencyMS:  msFromDuration(a.ReadLatency.P95),
		P95WriteLatencyMS: msFromDuration(a.WriteLatency.P95),
		LatencyBaselineMS: msFromDuration(a.Baseline.P95),

		VMHeadroomIOPS:  d.VMHeadroomIOPS,
		BudgetRemaining: d.BudgetRemaining,
		WindowFillUp:    a.WindowFillUpRatio,
		WindowFillDown:  a.WindowFillDownRatio,

		IOPSDemandDown:               a.IOPSP95Down,
		BandwidthDemandDownIOPSEquiv: a.BandwidthP95DownIOPSEquiv,
		LimitingResourceDown:         a.LimitingResourceDown,

		Source: string(a.Source),
	}
}

func msFromDuration(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// Writer appends one JSON object per line to out (JSON Lines) — append-only,
// matching how a log stream is consumed (tailed, shipped, grepped), not
// re-read and rewritten. Concurrency is the caller's responsibility, same
// as any io.Writer.
type Writer struct {
	enc *json.Encoder
}

func NewWriter(out io.Writer) *Writer {
	return &Writer{enc: json.NewEncoder(out)}
}

// Write appends r as one line. json.Encoder.Encode already writes a
// trailing newline after each value, which is exactly the JSON Lines
// framing this format needs — no extra separator to add.
func (w *Writer) Write(r Record) error {
	return w.enc.Encode(r)
}
