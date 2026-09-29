package member

import (
	"math"
	"time"
)

type IterationMeasurement struct {
	Method        string  `json:"method"`
	StartStep     int64   `json:"startStep"`
	EndStep       int64   `json:"endStep"`
	Samples       int64   `json:"samples"`
	MeanSeconds   float64 `json:"meanSeconds"`
	ObservedAt    string  `json:"observedAt"`
	WorkerSession string  `json:"workerSession"`
}

func (m *IterationMeasurement) status() map[string]interface{} {
	return map[string]interface{}{"method": m.Method, "startStep": m.StartStep,
		"endStep": m.EndStep, "samples": m.Samples, "meanSeconds": m.MeanSeconds,
		"observedAt": m.ObservedAt, "workerSession": m.WorkerSession}
}

func validIterationMeasurement(s RuntimeObservation, now time.Time) bool {
	m := s.IterationMeasurement
	if m == nil || m.Method != "optimizer-update-window-v1" || m.WorkerSession == "" || m.WorkerSession != s.WorkerSession ||
		m.StartStep < 0 || m.EndStep <= m.StartStep || m.Samples != m.EndStep-m.StartStep ||
		m.EndStep > s.GlobalStep || m.MeanSeconds <= 0 || math.IsNaN(m.MeanSeconds) || math.IsInf(m.MeanSeconds, 0) {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, m.ObservedAt)
	return err == nil && now.Sub(at) <= 60*time.Second && at.Sub(now) <= 5*time.Second
}

func aggregateIterationTiming(status map[string]interface{}, observations []RuntimeObservation, now time.Time) {
	// Never promote legacy optimizer-call latency or mismatched rank windows.
	delete(status, "iterationTimeSeconds")
	delete(status, "iterationMeasurement")
	if len(observations) == 0 {
		return
	}
	var first *IterationMeasurement
	var maximum float64
	var oldest time.Time
	for _, s := range observations {
		if !validIterationMeasurement(s, now) {
			return
		}
		m := s.IterationMeasurement
		at, _ := time.Parse(time.RFC3339Nano, m.ObservedAt)
		if first == nil {
			first, oldest = m, at
		} else if m.StartStep != first.StartStep || m.EndStep != first.EndStep || m.Samples != first.Samples {
			return
		}
		if at.Before(oldest) {
			oldest = at
		}
		maximum = math.Max(maximum, m.MeanSeconds)
	}
	status["iterationTimeSeconds"] = maximum
	status["iterationMeasurement"] = map[string]interface{}{
		"method": first.Method, "aggregation": "max-rank-mean", "startStep": first.StartStep,
		"endStep": first.EndStep, "samples": first.Samples, "rankCount": int64(len(observations)),
		"observedAt": oldest.UTC().Format(time.RFC3339Nano),
	}
}
