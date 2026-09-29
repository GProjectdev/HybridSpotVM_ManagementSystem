package member

import (
	"math"
	"testing"
	"time"
)

func TestIterationAggregationRequiresFreshAlignedFullWindows(t *testing.T) {
	now := time.Now()
	makeSample := func(seconds float64) RuntimeObservation {
		return RuntimeObservation{GlobalStep: 45, WorkerSession: "worker", IterationMeasurement: &IterationMeasurement{
			Method: "optimizer-update-window-v1", StartStep: 20, EndStep: 40, Samples: 20,
			MeanSeconds: seconds, ObservedAt: now.Format(time.RFC3339Nano), WorkerSession: "worker",
		}}
	}
	for _, tc := range []struct {
		name   string
		change func(*RuntimeObservation)
	}{
		{"missing", func(s *RuntimeObservation) { s.IterationMeasurement = nil }},
		{"legacy", func(s *RuntimeObservation) { s.IterationMeasurement.Method = "optimizer-step" }},
		{"restart", func(s *RuntimeObservation) { s.WorkerSession = "new-worker" }},
		{"stale", func(s *RuntimeObservation) {
			s.IterationMeasurement.ObservedAt = now.Add(-time.Minute - time.Second).Format(time.RFC3339Nano)
		}},
		{"skewed-window", func(s *RuntimeObservation) { s.IterationMeasurement.StartStep = 0; s.IterationMeasurement.EndStep = 20 }},
		{"partial", func(s *RuntimeObservation) { s.IterationMeasurement.Samples = 19 }},
		{"nan", func(s *RuntimeObservation) { s.IterationMeasurement.MeanSeconds = math.NaN() }},
		{"future-step", func(s *RuntimeObservation) { s.GlobalStep = 10 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := makeSample(2)
			tc.change(&s)
			status := map[string]interface{}{"iterationTimeSeconds": 99.0}
			aggregateIterationTiming(status, []RuntimeObservation{makeSample(1), s}, now)
			if _, ok := status["iterationTimeSeconds"]; ok {
				t.Fatal("invalid timing published")
			}
		})
	}
	status := map[string]interface{}{}
	aggregateIterationTiming(status, []RuntimeObservation{makeSample(1), makeSample(2)}, now)
	if status["iterationTimeSeconds"] != 2.0 {
		t.Fatalf("expected conservative max rank mean: %v", status)
	}
}
