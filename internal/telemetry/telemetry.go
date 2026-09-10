// Package telemetry exports Prometheus metrics from decisions and confirmed writes.
// Metrics without a real source are intentionally not registered.
package telemetry

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

// Registry owns an isolated Prometheus registry so instances and tests cannot collide.
type Registry struct {
	reg *prometheus.Registry

	provisionedIOPS         *prometheus.GaugeVec
	proposedTargetIOPS      *prometheus.GaugeVec
	observedIOPS            *prometheus.GaugeVec
	utilizationRatio        *prometheus.GaugeVec
	estimatedDemand         *prometheus.GaugeVec
	latencyBaseline         *prometheus.GaugeVec
	saturated               *prometheus.GaugeVec
	decisionsTotal          *prometheus.CounterVec
	vmHeadroom              *prometheus.GaugeVec
	vmCapReached            *prometheus.GaugeVec
	metricSource            *prometheus.GaugeVec
	metricStaleness         *prometheus.GaugeVec
	windowFillRatio         *prometheus.GaugeVec
	changeBudgetRemaining   *prometheus.GaugeVec
	discoverySkippedTotal   *prometheus.CounterVec
	diskConfiguredUnmanaged *prometheus.GaugeVec
	budgetHealthy           prometheus.Gauge

	// configuredUnmanagedMu guards configuredUnmanaged only — every other
	// field above relies on the underlying prometheus types' own thread
	// safety and needs no lock of its own. This one gauge's label set is
	// unbounded and operator-controlled (device_name), unlike every other
	// metric here, so SyncDiskConfiguredUnmanaged has to remember what it
	// last set in order to delete what's no longer current.
	configuredUnmanagedMu sync.Mutex
	configuredUnmanaged   map[[2]string]bool
}

func NewRegistry() *Registry {
	r := &Registry{reg: prometheus.NewRegistry()}

	r.provisionedIOPS = r.gauge("iops_autoscaler_disk_provisioned_iops", "Provisioned IOPS limit currently in effect — RecordDecision sets Decision.Current (issue #19: never the proposed target); RecordAppliedIOPS updates it once a write is verified to have landed.", "disk", "vm", "class")
	r.proposedTargetIOPS = r.gauge("iops_autoscaler_disk_proposed_target_iops", "Decider's proposed new limit this cycle (Decision.Target) — decision intent, not applied state (issue #19). Equals disk_provisioned_iops on a NoAction cycle.", "disk", "vm", "class")
	r.observedIOPS = r.gauge("iops_autoscaler_disk_observed_iops", "Latest observed ops/s.", "disk", "vm", "op")
	r.utilizationRatio = r.gauge("iops_autoscaler_disk_utilization_ratio", "Latest IOPS utilization ratio.", "disk", "vm")
	r.estimatedDemand = r.gauge("iops_autoscaler_disk_estimated_demand_iops", "Decider's estimated demand, IOPS.", "disk", "vm")
	r.latencyBaseline = r.gauge("iops_autoscaler_disk_latency_baseline_seconds", "Disk's own low-percentile latency baseline.", "disk", "vm")
	r.saturated = r.gauge("iops_autoscaler_disk_saturated", "1 if the disk is saturated this cycle, else 0.", "disk", "vm")
	r.decisionsTotal = r.counter("iops_autoscaler_decisions_total", "Reconcile decisions by action and reason.", "action", "reason")
	r.vmHeadroom = r.gauge("iops_autoscaler_vm_headroom_iops", "IOPS pool headroom available to this specific disk: VM pool minus the p95 consumption of its siblings (design.md §4.6) — disk-relative, hence the disk label (issue #19).", "disk", "vm")
	r.vmCapReached = r.gauge("iops_autoscaler_vm_cap_reached", "1 if any managed disk on this VM ended its cycle in VM_IOPS_CAP_REACHED, else 0 — set once per reconcile via RecordVMCapReached, never per-disk (issue #19: a per-disk Set here let disk iteration order silently clear this).", "vm")
	r.metricSource = r.gauge("iops_autoscaler_metric_source", "1 for the source (guest|platform) that produced this cycle's Analysis, 0 for the other — both series are always set together so a source switch cannot leave a stale active series behind (issue #19).", "disk", "source")
	r.metricStaleness = r.gauge("iops_autoscaler_metric_staleness_seconds", "Age of the sample this decision was based on.", "disk")
	r.windowFillRatio = r.gauge("iops_autoscaler_window_fill_ratio", "Fraction of the configured window currently buffered.", "disk", "direction")
	r.changeBudgetRemaining = r.gauge("iops_autoscaler_change_budget_remaining", "Changes remaining in the trailing 24h budget.", "disk")
	r.discoverySkippedTotal = r.counter("iops_autoscaler_disk_discovery_skipped_total", "Disk attachments excluded from the managed set, by reason.", "alias", "reason")
	r.diskConfiguredUnmanaged = r.gauge("iops_autoscaler_disk_configured_unmanaged", "1 while a disks.selected or disks.exclude entry named device_name cannot be resolved against the VM's live disks (docs/design.md §4.1) — the series is deleted, not set to 0, once it resolves again (SyncDiskConfiguredUnmanaged), so a resolved disk disappears from this metric entirely rather than leaving a stale 'still degraded' reading.", "device_name", "reason")
	r.budgetHealthy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iops_autoscaler_budget_healthy",
		Help: "1 if the durable 24h change budget can currently be trusted, else 0 (issue #10).",
	})
	r.reg.MustRegister(r.budgetHealthy)

	return r
}

func (r *Registry) gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	r.reg.MustRegister(g)
	return g
}

func (r *Registry) counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	r.reg.MustRegister(c)
	return c
}

// Handler serves the Prometheus text exposition format.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// RecordDecision updates per-disk decision metrics before actuation.
// Provisioned IOPS therefore uses Current, while Target is exposed separately as intent.
// VM-wide cap state and confirmed applied values are recorded by their dedicated methods.
func (r *Registry) RecordDecision(vmName, class string, d decider.Decision) {
	disk := d.DiskID
	a := d.Inputs

	r.provisionedIOPS.WithLabelValues(disk, vmName, class).Set(float64(d.Current))
	r.proposedTargetIOPS.WithLabelValues(disk, vmName, class).Set(float64(d.Target))
	r.observedIOPS.WithLabelValues(disk, vmName, "read").Set(a.ReadOps)
	r.observedIOPS.WithLabelValues(disk, vmName, "write").Set(a.WriteOps)
	r.utilizationRatio.WithLabelValues(disk, vmName).Set(a.IOPSUtilization)
	r.estimatedDemand.WithLabelValues(disk, vmName).Set(d.Demand)
	r.latencyBaseline.WithLabelValues(disk, vmName).Set(a.Baseline.P95.Seconds())
	r.saturated.WithLabelValues(disk, vmName).Set(boolFloat(a.Saturated))
	r.decisionsTotal.WithLabelValues(string(d.Action), d.Reason).Inc()
	r.vmHeadroom.WithLabelValues(disk, vmName).Set(float64(d.VMHeadroomIOPS))

	// Set both source labels so a previously active GaugeVec series cannot remain at one.
	r.metricSource.WithLabelValues(disk, string(metrics.SourceGuest)).Set(boolFloat(a.Source == metrics.SourceGuest))
	r.metricSource.WithLabelValues(disk, string(metrics.SourcePlatform)).Set(boolFloat(a.Source == metrics.SourcePlatform))

	r.windowFillRatio.WithLabelValues(disk, "up").Set(a.WindowFillUpRatio)
	r.windowFillRatio.WithLabelValues(disk, "down").Set(a.WindowFillDownRatio)
	r.changeBudgetRemaining.WithLabelValues(disk).Set(float64(d.BudgetRemaining))
}

// RecordAppliedIOPS updates provisioned state only after the caller verifies a write.
func (r *Registry) RecordAppliedIOPS(diskID, vmName, class string, appliedIOPS int64) {
	r.provisionedIOPS.WithLabelValues(diskID, vmName, class).Set(float64(appliedIOPS))
}

// RecordVMCapReached records the cycle-wide OR across all disks on a VM.
func (r *Registry) RecordVMCapReached(vmName string, capped bool) {
	r.vmCapReached.WithLabelValues(vmName).Set(boolFloat(capped))
}

// RecordStaleness reports sampling age even when no decision can be produced.
func (r *Registry) RecordStaleness(diskID string, seconds float64) {
	r.metricStaleness.WithLabelValues(diskID).Set(seconds)
}

// RecordDiscoverySkipped counts excluded attachments by alias and stable reason category.
func (r *Registry) RecordDiscoverySkipped(alias, reason string) {
	r.discoverySkippedTotal.WithLabelValues(alias, reason).Inc()
}

// SyncDiskConfiguredUnmanaged replaces the full current set of degraded
// disks with current (device_name -> reason), called every discovery cycle
// including with an empty map. Entries no longer present are deleted, not
// zeroed, so a disk that resolves again disappears from the metric
// entirely instead of leaving a stale "still degraded" series behind
// (issue #19's discipline, extended here to deletion since this is the one
// gauge in this file whose label set is unbounded and operator-controlled
// rather than fixed in advance).
func (r *Registry) SyncDiskConfiguredUnmanaged(current map[string]string) {
	r.configuredUnmanagedMu.Lock()
	defer r.configuredUnmanagedMu.Unlock()

	next := make(map[[2]string]bool, len(current))
	for device, reason := range current {
		key := [2]string{device, reason}
		next[key] = true
		if !r.configuredUnmanaged[key] {
			r.diskConfiguredUnmanaged.WithLabelValues(device, reason).Set(1)
		}
	}
	for key := range r.configuredUnmanaged {
		if !next[key] {
			r.diskConfiguredUnmanaged.DeleteLabelValues(key[0], key[1])
		}
	}
	r.configuredUnmanaged = next
}

// RecordBudgetHealthy exports the tracker's durable-budget health without coupling state to telemetry.
func (r *Registry) RecordBudgetHealthy(healthy bool) {
	r.budgetHealthy.Set(boolFloat(healthy))
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
