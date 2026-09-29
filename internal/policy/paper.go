package policy

import (
	"fmt"
	"math"
	"time"
)

// PaperProfile describes measured async checkpoint costs, not CRIU/export latency.
type PaperProfile struct {
	Enabled             bool
	Asynchronous        bool
	GPUToDRAMSeconds    float64
	StorageSeconds      float64
	CheckpointGiB       float64
	BufferGiB           float64
	ObservedAt          string
	CandidateIterations []int64
}

type PaperInterval struct {
	Iterations              int64
	Seconds                 int64
	CostSecondsPerIteration float64
}

// PaperCheckpointCost implements Cost-Efficient section 4.2, equations 1-4.
// iterationSeconds is measured T/Ndp. T is reconstructed exactly once for lambda_iter.
func PaperCheckpointCost(f int64, iterationSeconds float64, dpShards int64, lambdaPerHour float64, p PaperProfile) (float64, error) {
	if f <= 0 || dpShards <= 0 || !finite(iterationSeconds) || iterationSeconds <= 0 ||
		!finite(lambdaPerHour) || lambdaPerHour < 0 || !p.Asynchronous ||
		!finite(p.GPUToDRAMSeconds) || p.GPUToDRAMSeconds < 0 ||
		!finite(p.StorageSeconds) || p.StorageSeconds < 0 ||
		!finite(p.CheckpointGiB) || p.CheckpointGiB <= 0 || !finite(p.BufferGiB) || p.BufferGiB < p.CheckpointGiB {
		return 0, fmt.Errorf("invalid or non-asynchronous paper model inputs")
	}
	minimum := math.Ceil(p.StorageSeconds * p.CheckpointGiB / (p.BufferGiB * iterationSeconds))
	if float64(f) < minimum {
		return 0, fmt.Errorf("candidate violates paper DRAM buffer constraint")
	}
	lambdaIteration := lambdaPerHour * iterationSeconds * float64(dpShards) / 3600
	cost := iterationSeconds + p.GPUToDRAMSeconds/float64(f) +
		math.Max(0, p.StorageSeconds/float64(f)-iterationSeconds) +
		lambdaIteration*float64(f)*iterationSeconds/2
	if !finite(cost) {
		return 0, fmt.Errorf("non-finite paper objective")
	}
	return cost, nil
}

// SelectPaperInterval searches only user-bounded integer iteration candidates (Eq.5).
func SelectPaperInterval(cp CheckpointPolicy, rt RuntimeSnapshot, risk RiskSnapshot, now time.Time) (PaperInterval, error) {
	p := cp.Paper
	at, err := time.Parse(time.RFC3339Nano, p.ObservedAt)
	if !p.Enabled || err != nil || at.After(now) || now.Sub(at) > 10*time.Minute {
		return PaperInterval{}, fmt.Errorf("fresh measured async checkpoint profile required")
	}
	timingAt, err := time.Parse(time.RFC3339Nano, rt.IterationObservedAt)
	if err != nil || now.Sub(timingAt) > time.Minute || timingAt.After(now) ||
		rt.IterationMethod != "optimizer-update-window-v1" || rt.IterationAggregation != "max-rank-mean" ||
		rt.IterationRankCount != rt.WorldSize || rt.WorldSize <= 0 || rt.ReadyRanks != rt.WorldSize ||
		rt.IterationSamples <= 0 || rt.IterationEndStep-rt.IterationStartStep != rt.IterationSamples ||
		rt.IterationEndStep > rt.GlobalStep || !risk.Ready {
		return PaperInterval{}, fmt.Errorf("fresh aligned world iteration measurement required")
	}
	best := PaperInterval{}
	for _, f := range p.CandidateIterations {
		cost, err := PaperCheckpointCost(f, rt.IterationTimeSeconds, rt.WorldSize, risk.LambdaPerHour, p)
		if err != nil {
			continue
		}
		seconds := math.Ceil(float64(f) * rt.IterationTimeSeconds)
		if !finite(seconds) || seconds <= 0 || seconds >= float64(math.MaxInt64) {
			continue
		}
		interval := int64(seconds)
		if (cp.MinIntervalSeconds > 0 && interval < cp.MinIntervalSeconds) ||
			(cp.MaxIntervalSeconds > 0 && interval > cp.MaxIntervalSeconds) {
			continue
		}
		if best.Iterations == 0 || cost < best.CostSecondsPerIteration ||
			(cost == best.CostSecondsPerIteration && f < best.Iterations) {
			best = PaperInterval{Iterations: f, Seconds: interval, CostSecondsPerIteration: cost}
		}
	}
	if best.Iterations == 0 {
		return best, fmt.Errorf("no feasible paper interval candidate")
	}
	return best, nil
}
