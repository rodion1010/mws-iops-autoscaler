// Package telemetry exports the Prometheus metrics design.md §7.1 defines,
// derived from a decider.Decision (which already carries its full
// analyzer.Analysis in Inputs). It wires every metric except
// writes_total/write_duration_seconds (no Actuator exists yet to report a
// write outcome — M6) and cost_delta_iops_hours (no billing-rate model is
// designed anywhere yet) — registering a metric with nothing real behind it
// would repeat the exact *_quota_utilization-reads-zero trap docs/platform-reference.md
// documents as a platform bug, not something to imitate.
package telemetry

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

// Registry holds one isolated prometheus.Registry (never the global
// DefaultRegisterer) so multiple instances never collide in tests and a
// caller controls exactly what's exposed on /metrics.
type Registry struct {
	reg *prometheus.Registry

	provisionedIOPS       *prometheus.GaugeVec
	proposedTargetIOPS    *prometheus.GaugeVec
	observedIOPS          *prometheus.GaugeVec
	utilizationRatio      *prometheus.GaugeVec
	estimatedDemand       *prometheus.GaugeVec
	latencyBaseline       *prometheus.GaugeVec
	saturated             *prometheus.GaugeVec
	decisionsTotal        *prometheus.CounterVec
	vmHeadroom            *prometheus.GaugeVec
	vmCapReached          *prometheus.GaugeVec
	metricSource          *prometheus.GaugeVec
	metricStaleness       *prometheus.GaugeVec
	windowFillRatio       *prometheus.GaugeVec
	changeBudgetRemaining *prometheus.GaugeVec
	discoverySkippedTotal *prometheus.CounterVec
	budgetHealthy         prometheus.Gauge
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

// Handler serves the Prometheus text exposition format for everything
// registered here.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// RecordDecision updates every gauge/counter this package owns from one
// reconcile cycle's Decision (which already carries its Analysis in
// Inputs), except vm_cap_reached (RecordVMCapReached, aggregated once per
// reconcile across every disk on the VM) and the applied-state half of
// disk_provisioned_iops (RecordAppliedIOPS, called only once a write is
// verified to have landed). vmName and class (dependent|independent,
// ADR-003) come from the caller — neither is on Decision, since Decide has
// no reason to know either (design.md §3.3's decoupling: Decide only needs
// policy/state/evidence, not disk-classification metadata a journal
// consumer wants).
//
// disk_provisioned_iops is set from d.Current here, not d.Target (issue
// #19) — this call fires before actuate() ever attempts a write, so
// reporting the proposed target as already provisioned would be wrong on
// every dry-run cycle, every failed write, and for the whole window while
// a write is in flight. d.Target is exposed separately, unconditionally,
// as disk_proposed_target_iops — decision intent, distinguishable from
// applied state by metric name rather than by cross-referencing the
// journal.
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

	// Both source series are always set together (issue #19) — a GaugeVec
	// only ever remembers series someone has Set(), so the previously
	// active source's series would otherwise stay at 1 forever once the
	// active source switches; explicitly zeroing the inactive one each
	// call keeps exactly one series at 1 per disk, always.
	r.metricSource.WithLabelValues(disk, string(metrics.SourceGuest)).Set(boolFloat(a.Source == metrics.SourceGuest))
	r.metricSource.WithLabelValues(disk, string(metrics.SourcePlatform)).Set(boolFloat(a.Source == metrics.SourcePlatform))

	r.windowFillRatio.WithLabelValues(disk, "up").Set(a.WindowFillUpRatio)
	r.windowFillRatio.WithLabelValues(disk, "down").Set(a.WindowFillDownRatio)
	r.changeBudgetRemaining.WithLabelValues(disk).Set(float64(d.BudgetRemaining))
}

// RecordAppliedIOPS updates disk_provisioned_iops to a write the caller has
// already verified landed (issue #19) — never called for a dry run or a
// failed write, so the gauge only ever moves on confirmed applied state,
// same class/label shape as RecordDecision's own Set of this gauge so the
// two calls target the same series.
func (r *Registry) RecordAppliedIOPS(diskID, vmName, class string, appliedIOPS int64) {
	r.provisionedIOPS.WithLabelValues(diskID, vmName, class).Set(float64(appliedIOPS))
}

// RecordVMCapReached sets vm_cap_reached once per reconcile cycle, from the
// caller's own OR across every disk decided that cycle (issue #19) — never
// from a single disk's Decision, which is exactly the per-disk Set that let
// disk iteration order silently clear this gauge (a VM where disk A capped
// and disk B, processed later, did not would previously end the cycle
// reporting 0).
func (r *Registry) RecordVMCapReached(vmName string, capped bool) {
	r.vmCapReached.WithLabelValues(vmName).Set(boolFloat(capped))
}

// RecordStaleness sets metric_staleness_seconds directly — it needs the
// sampler's own notion of "time since last successful sample," which
// Analysis does not carry (Analysis only ever exists once a sample landed;
// staleness is about the *absence* of one). Left as a separate call rather
// than folded into RecordDecision so a caller with no fresh decision this
// cycle (a disk marked "no data") can still report it.
func (r *Registry) RecordStaleness(diskID string, seconds float64) {
	r.metricStaleness.WithLabelValues(diskID).Set(seconds)
}

// RecordDiscoverySkipped increments disk_discovery_skipped_total for one
// excluded attachment (issue #4) — alias, not DiskID/DiskName, since a
// disk skipped for an empty or duplicate resource identity may not have a
// usable DiskName to label with; alias is always present (it's the VM
// spec's own collection key). reason must be one of the compute package's
// SkipReason* constants, not free text — see that package's own comment on
// why Category exists separately from the free-text Reason.
func (r *Registry) RecordDiscoverySkipped(alias, reason string) {
	r.discoverySkippedTotal.WithLabelValues(alias, reason).Inc()
}

// RecordBudgetHealthy mirrors state.Tracker.Healthy() into
// iops_autoscaler_budget_healthy (issue #10) — polled once per reconcile
// cycle rather than pushed from Tracker itself, keeping Tracker decoupled
// from telemetry the same way every other pure package here stays
// decoupled from its observers.
func (r *Registry) RecordBudgetHealthy(healthy bool) {
	r.budgetHealthy.Set(boolFloat(healthy))
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
