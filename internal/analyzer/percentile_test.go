package analyzer

import (
	"testing"
	"time"
)

func durations(ms ...int) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, m := range ms {
		out[i] = time.Duration(m) * time.Millisecond
	}
	return out
}

func TestPercentile_empty(t *testing.T) {
	if got := percentile(nil, 0.95); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
}

func TestPercentile_knownSeries(t *testing.T) {
	// 1..20 ms, nearest-rank: idx = ceil(p*n)-1.
	// p10 -> ceil(2)-1=1  -> value 2
	// p50 -> ceil(10)-1=9 -> value 10
	// p95 -> ceil(19)-1=18 -> value 19
	// p100 -> ceil(20)-1=19 -> value 20
	series := make([]time.Duration, 20)
	for i := range series {
		series[i] = time.Duration(i+1) * time.Millisecond
	}
	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0.10, 2 * time.Millisecond},
		{0.50, 10 * time.Millisecond},
		{0.95, 19 * time.Millisecond},
		{1.00, 20 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := percentile(series, tc.p); got != tc.want {
			t.Errorf("percentile(series, %v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestPercentile_unsortedInputSortedInternally(t *testing.T) {
	series := durations(9, 1, 5, 3, 7)
	if got, want := percentile(series, 1.0), 9*time.Millisecond; got != want {
		t.Errorf("percentile(unsorted, 1.0) = %v, want %v", got, want)
	}
	// Original slice must not be mutated.
	if series[0] != 9*time.Millisecond {
		t.Errorf("percentile mutated its input: %v", series)
	}
}

func TestPercentile_singleValue(t *testing.T) {
	series := durations(42)
	for _, p := range []float64{0.1, 0.5, 0.95, 1.0} {
		if got, want := percentile(series, p), 42*time.Millisecond; got != want {
			t.Errorf("percentile(single, %v) = %v, want %v", p, got, want)
		}
	}
}

func TestPercentileFloat_knownSeries(t *testing.T) {
	series := []float64{100, 200, 300, 400, 500}
	if got, want := percentileFloat(series, 0.95), 500.0; got != want {
		t.Errorf("percentileFloat(series, 0.95) = %v, want %v", got, want)
	}
	if got, want := percentileFloat(series, 0.5), 300.0; got != want {
		t.Errorf("percentileFloat(series, 0.5) = %v, want %v", got, want)
	}
}

func TestPercentileFloat_empty(t *testing.T) {
	if got := percentileFloat(nil, 0.95); got != 0 {
		t.Errorf("percentileFloat(nil) = %v, want 0", got)
	}
}
