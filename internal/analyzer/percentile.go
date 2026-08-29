package analyzer

import (
	"math"
	"sort"
	"time"
)

// percentile returns the nearest-rank p-th percentile (p in [0,1]) of
// values, which need not be pre-sorted — this function sorts a copy.
// Nearest-rank (not linear interpolation) is chosen for determinism on
// small windows: design.md's own examples (p95 on a handful of samples)
// don't have enough points for interpolation to mean much more than
// nearest-rank already gives, and nearest-rank never invents a value that
// wasn't actually observed. Empty input returns zero.
func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// percentileFloat is percentile's float64 counterpart, for ops/s rather
// than latency — same nearest-rank method, same determinism rationale.
func percentileFloat(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
