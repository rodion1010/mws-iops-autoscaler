package config

import (
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
)

// An accepted boundary configuration must eventually produce an evaluable window.
func TestRegressionEveryAcceptedCollectionCadenceCanFillItsWindow(t *testing.T) {
	cfg := validConfig()
	cfg.Runtime.Metrics.CollectionInterval = durationPtr("5m")
	cfg.Policy.Advanced = &AdvancedSpec{
		Evaluation: &EvaluationSpec{
			ScaleUpWindow: durationPtr("5m"),
		},
	}

	if err := Validate(cfg); err != nil {
		return
	}

	now := time.Unix(600, 0)
	samples := []metrics.Sample{
		{DiskID: "disk", Timestamp: now.Add(-10 * time.Minute), Source: metrics.SourceGuest},
		{DiskID: "disk", Timestamp: now.Add(-5 * time.Minute), Source: metrics.SourceGuest},
		{DiskID: "disk", Timestamp: now, Source: metrics.SourceGuest},
	}
	analysis := analyzer.NewWindowAnalyzer().Analyze("disk", samples, analyzer.EvaluationPolicy{
		ScaleUpWindow:             5 * time.Minute,
		ScaleDownWindow:           30 * time.Minute,
		UtilizationThreshold:      0.95,
		QueueDepthThreshold:       1,
		LatencyBaselineMultiplier: 1.5,
		MaxSampleAge:              time.Minute,
		MaxSampleGap:              5 * time.Minute,
	}, 1000, now)
	if !analysis.WindowFullUp {
		t.Fatal("Validate accepted collection_interval == scale_up_window, but three exact-cadence samples still produced WindowFullUp=false")
	}
}
