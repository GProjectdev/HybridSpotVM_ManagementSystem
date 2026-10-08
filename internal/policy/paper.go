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
	CandidateIterations []int64 // Deprecated: accepted for old manifests, not used by the solver.
}

type PaperInterval struct {
	Iterations              int64
	Seconds                 int64
	CostSecondsPerIteration float64
}

// PaperCheckpointCost implements Cost-Efficient section 4.2, equations 1-4.
// iterationSeconds is measured T/Ndp. T is reconstructed exactly once for lambda_iter.
func PaperCheckpointCost(f int64, iterationSeconds float64, dpShards int64, lambdaPerHour float64, p PaperProfile) (float64, error) {
	if f <= 0 {
		return 0, fmt.Errorf("checkpoint iterations must be positive")
	}
	if err := validatePaperModel(iterationSeconds, dpShards, lambdaPerHour, p); err != nil {
		return 0, err
	}
	minimum := paperMemoryMinimum(iterationSeconds, p)
	if !finite(minimum) || float64(f) < minimum {
		return 0, fmt.Errorf("interval violates paper DRAM buffer constraint")
	}
	lambdaIteration := lambdaPerHour / 3600 * iterationSeconds * float64(dpShards)
	cost := iterationSeconds + p.GPUToDRAMSeconds/float64(f) +
		math.Max(0, p.StorageSeconds/float64(f)-iterationSeconds) +
		lambdaIteration*float64(f)*iterationSeconds/2
	if !finite(cost) {
		return 0, fmt.Errorf("non-finite paper objective")
	}
	return cost, nil
}

func validatePaperModel(iterationSeconds float64, dpShards int64, lambdaPerHour float64, p PaperProfile) error {
	if dpShards <= 0 || !finite(iterationSeconds) || iterationSeconds <= 0 ||
		!finite(lambdaPerHour) || lambdaPerHour < 0 || !p.Asynchronous ||
		!finite(p.GPUToDRAMSeconds) || p.GPUToDRAMSeconds < 0 ||
		!finite(p.StorageSeconds) || p.StorageSeconds < 0 ||
		!finite(p.CheckpointGiB) || p.CheckpointGiB <= 0 || !finite(p.BufferGiB) || p.BufferGiB < p.CheckpointGiB {
		return fmt.Errorf("invalid or non-asynchronous paper model inputs")
	}
	return nil
}

func paperMemoryMinimum(iterationSeconds float64, p PaperProfile) float64 {
	return math.Ceil((p.CheckpointGiB / p.BufferGiB) * p.StorageSeconds / iterationSeconds)
}

// SelectPaperInterval solves the integer argmin in Eq.5 using the piecewise
// derivative of Eq.3, rather than an operator-supplied list of candidates.
// This analytical solver is a derivation; the paper itself describes a sweep.
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
	t := rt.IterationTimeSeconds // Already T/Ndp; do not divide by world size again.
	if err := validatePaperModel(t, rt.WorldSize, risk.LambdaPerHour, p); err != nil {
		return PaperInterval{}, err
	}
	minSeconds, maxSeconds, err := checkpointIntervalBounds(cp)
	if err != nil {
		return PaperInterval{}, err
	}
	// The downstream API schedules whole seconds using ceil(f*t).
	const maxIterations int64 = 1<<53 - 1
	memoryMinimum := math.Max(1, paperMemoryMinimum(t, p))
	lower := math.Max(memoryMinimum, math.Floor(float64(minSeconds-1)/t)+1)
	upper := math.Floor(float64(maxSeconds) / t)
	if !finite(lower) || !finite(upper) || lower > float64(maxIterations+1) || upper > float64(maxIterations) {
		return PaperInterval{}, fmt.Errorf("no representable paper interval satisfies memory and time bounds")
	}
	lo, hi := int64(lower), int64(upper)
	// Division and multiplication can round opposite ways at whole seconds.
	// Correct only neighboring bounds using the scheduler's actual mapping.
	for lo > int64(memoryMinimum) && math.Ceil(float64(lo-1)*t) >= float64(minSeconds) {
		lo--
	}
	for lo <= maxIterations && math.Ceil(float64(lo)*t) < float64(minSeconds) {
		lo++
	}
	for hi < maxIterations && math.Ceil(float64(hi+1)*t) <= float64(maxSeconds) {
		hi++
	}
	for hi > 0 && math.Ceil(float64(hi)*t) > float64(maxSeconds) {
		hi--
	}
	if lo > hi {
		return PaperInterval{}, fmt.Errorf("no representable paper interval satisfies memory and time bounds")
	}
	lambdaIteration := risk.LambdaPerHour / 3600 * t * float64(rt.WorldSize)
	f, err := optimalIntegerInterval(p.GPUToDRAMSeconds, p.StorageSeconds, t, lambdaIteration*t/2, lo, hi)
	if err != nil {
		return PaperInterval{}, err
	}
	cost, err := PaperCheckpointCost(f, t, rt.WorldSize, risk.LambdaPerHour, p)
	seconds := math.Ceil(float64(f) * t)
	if err != nil || !finite(seconds) || seconds < float64(minSeconds) || seconds > float64(maxSeconds) {
		return PaperInterval{}, fmt.Errorf("calculated paper interval violates bounds or objective: %v", err)
	}
	return PaperInterval{Iterations: f, Seconds: int64(seconds), CostSecondsPerIteration: cost}, nil
}
