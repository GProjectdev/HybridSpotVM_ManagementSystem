package policy

import (
	"math"
	"testing"
	"time"
)

func TestMeasuredAnalyticIntervalDoesNotSnapToCandidates(t *testing.T) {
	now := time.Now().UTC()
	cp := CheckpointPolicy{MinIntervalSeconds: 1, MaxIntervalSeconds: 600,
		CandidateIntervals: []int64{60, 120, 300, 600},
		MeasuredCosts:      MeasuredCosts{CheckpointSeconds: 12, CopySeconds: 0, ObservedAt: now.Format(time.RFC3339Nano)}}
	// sqrt(2*12/(.8/3600)) = 328.63 seconds; integer optimum is 329.
	seconds, evaluated := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true, LambdaPerHour: .8}, RuntimeSnapshot{}, 1, now)
	if !evaluated || seconds != 329 {
		t.Fatalf("got %d, evaluated=%v; want direct integer optimum 329", seconds, evaluated)
	}
	cp.CandidateIntervals = nil
	withoutList, ok := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true, LambdaPerHour: .8}, RuntimeSnapshot{}, 1, now)
	if !ok || withoutList != seconds {
		t.Fatal("deprecated candidates affect the result")
	}
	for _, spot := range []int64{0, 1} {
		got, ok := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true, LambdaPerHour: 0}, RuntimeSnapshot{}, spot, now)
		if !ok || got != 600 {
			t.Fatalf("zero hazard must use finite cap: %d %v", got, ok)
		}
	}
	if got, ok := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true, LambdaPerHour: .8}, RuntimeSnapshot{}, 0, now); !ok || got != 600 {
		t.Fatalf("no Spot workers must use cap: %d %v", got, ok)
	}
	cp.MinIntervalSeconds, cp.MaxIntervalSeconds = 137, 137
	if got, ok := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true, LambdaPerHour: .8}, RuntimeSnapshot{}, 1, now); !ok || got != 137 {
		t.Fatalf("fixed bounds lost: %d %v", got, ok)
	}
}

func TestAnalyticBootstrapUsesBoundsNotLegacyCandidates(t *testing.T) {
	cp := CheckpointPolicy{MinIntervalSeconds: 10, MaxIntervalSeconds: 500,
		CandidateIntervals: []int64{600}, RiskBands: []RiskBand{{MaxLambdaPerHour: 1, IntervalSeconds: 137}}}
	got, ok := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true, LambdaPerHour: .8}, RuntimeSnapshot{}, 1, time.Now())
	if ok || got != 137 {
		t.Fatalf("bootstrap should use declared band without candidate snapping: %d %v", got, ok)
	}
}

func paperTimingFixture(now time.Time, seconds float64) RuntimeSnapshot {
	return RuntimeSnapshot{WorldSize: 2, ReadyRanks: 2, GlobalStep: 40,
		IterationTimeSeconds: seconds, IterationMethod: "optimizer-update-window-v1",
		IterationAggregation: "max-rank-mean", IterationRankCount: 2,
		IterationStartStep: 20, IterationEndStep: 40, IterationSamples: 20,
		IterationObservedAt: now.Format(time.RFC3339Nano)}
}

func TestPaperAnalyticOptimumWithoutOperatorCandidates(t *testing.T) {
	now := time.Now().UTC()
	cp := CheckpointPolicy{MinIntervalSeconds: 1, MaxIntervalSeconds: 600,
		Paper: PaperProfile{Enabled: true, Asynchronous: true, GPUToDRAMSeconds: 4,
			StorageSeconds: 12, CheckpointGiB: 2, BufferGiB: 4, ObservedAt: now.Format(time.RFC3339Nano)}}
	got, err := SelectPaperInterval(cp, paperTimingFixture(now, 2), RiskSnapshot{Ready: true, LambdaPerHour: .5}, now)
	if err != nil || got.Iterations != 85 || got.Seconds != 170 {
		t.Fatalf("paper optimum should be f=85, not a configured candidate: %+v %v", got, err)
	}
	cp.Paper.CandidateIterations = []int64{10, 20, 40}
	again, err := SelectPaperInterval(cp, paperTimingFixture(now, 2), RiskSnapshot{Ready: true, LambdaPerHour: .5}, now)
	if err != nil || again != got {
		t.Fatalf("legacy candidates changed optimum: %+v %v", again, err)
	}
}

func TestPaperAnalyticMatchesExhaustiveIntegerDomain(t *testing.T) {
	now := time.Now().UTC()
	for _, iteration := range []float64{.25, .7, 2, 7} {
		for _, dram := range []float64{0, 4, 30} {
			for _, storage := range []float64{0, 12, 100} {
				for _, risk := range []float64{0, .05, .8, 30, 3600} {
					cp := CheckpointPolicy{MinIntervalSeconds: 5, MaxIntervalSeconds: 61,
						Paper: PaperProfile{Enabled: true, Asynchronous: true, GPUToDRAMSeconds: dram,
							StorageSeconds: storage, CheckpointGiB: 1, BufferGiB: 4, ObservedAt: now.Format(time.RFC3339Nano)}}
					got, err := SelectPaperInterval(cp, paperTimingFixture(now, iteration), RiskSnapshot{Ready: true, LambdaPerHour: risk}, now)
					bestCost := math.Inf(1)
					for f := int64(1); float64(f)*iteration <= 61; f++ {
						if math.Ceil(float64(f)*iteration) < 5 {
							continue
						}
						cost, e := PaperCheckpointCost(f, iteration, 2, risk, cp.Paper)
						if e == nil && cost < bestCost {
							bestCost = cost
						}
					}
					if math.IsInf(bestCost, 1) {
						if err == nil {
							t.Fatal("accepted empty feasible domain")
						}
					} else if err != nil || math.Abs(got.CostSecondsPerIteration-bestCost) > 1e-10*math.Max(1, bestCost) {
						t.Fatalf("t=%g D=%g S=%g risk=%g got=%+v err=%v oracle=%g", iteration, dram, storage, risk, got, err, bestCost)
					}
				}
			}
		}
	}
}

func TestPaperAnalyticSchedulerRoundingBounds(t *testing.T) {
	const maxIterations int64 = 1<<53 - 1
	now := time.Now().UTC()
	tests := []struct {
		name               string
		iteration, storage float64
		min, max           int64
		lambda             float64
		want, seconds      int64
		wantError          bool
	}{
		{"raise lower", .017, 0, 18, 20, 1, 1001, 18, false},
		{"lower lower", .017, 0, 52, 54, 1, 3000, 52, false},
		{"raise upper", .017, 0, 1, 17, 0, 1000, 17, false},
		{"lower upper", .017, 0, 1, 51, 0, 2999, 51, false},
		{"memory minimum", .017, 18, 18, 20, 1, 1059, 19, false},
		{"memory infeasible", .017, 21, 18, 20, 1, 0, 0, true},
		{"exact integer limit", 1 / float64(maxIterations), 0, 1, 1, 0, maxIterations, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := CheckpointPolicy{MinIntervalSeconds: tt.min, MaxIntervalSeconds: tt.max,
				Paper: PaperProfile{Enabled: true, Asynchronous: true, GPUToDRAMSeconds: .001,
					StorageSeconds: tt.storage, CheckpointGiB: 1, BufferGiB: 1, ObservedAt: now.Format(time.RFC3339Nano)}}
			got, err := SelectPaperInterval(cp, paperTimingFixture(now, tt.iteration), RiskSnapshot{Ready: true, LambdaPerHour: tt.lambda}, now)
			if tt.wantError {
				if err == nil {
					t.Fatalf("accepted infeasible memory bound: %+v", got)
				}
				return
			}
			if err != nil || got.Iterations != tt.want || got.Seconds != tt.seconds {
				t.Fatalf("got %+v, err=%v; want f=%d, seconds=%d", got, err, tt.want, tt.seconds)
			}
		})
	}
}

func TestPaperRejectsInvalidOrExpiredInputs(t *testing.T) {
	now := time.Now().UTC()
	cp := CheckpointPolicy{Paper: PaperProfile{Enabled: true, Asynchronous: true, GPUToDRAMSeconds: 4,
		StorageSeconds: 12, CheckpointGiB: 1, BufferGiB: 4, ObservedAt: now.Format(time.RFC3339Nano)}}
	for _, timing := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, err := SelectPaperInterval(cp, paperTimingFixture(now, timing), RiskSnapshot{Ready: true, LambdaPerHour: .5}, now); err == nil {
			t.Fatalf("invalid iteration accepted: %g", timing)
		}
	}
	for _, age := range []time.Duration{-time.Second, 11 * time.Minute} {
		cp.Paper.ObservedAt = now.Add(-age).Format(time.RFC3339Nano)
		if _, err := SelectPaperInterval(cp, paperTimingFixture(now, 2), RiskSnapshot{Ready: true, LambdaPerHour: .5}, now); err == nil {
			t.Fatalf("invalid profile timestamp accepted: age=%v", age)
		}
	}
	cp.Paper.ObservedAt = now.Format(time.RFC3339Nano)
	if _, err := SelectPaperInterval(cp, paperTimingFixture(now.Add(-2*time.Minute), 2), RiskSnapshot{Ready: true, LambdaPerHour: .5}, now); err == nil {
		t.Fatal("stale iteration measurement accepted")
	}
	cp.MaxIntervalSeconds = math.MaxInt32 + 1
	if _, err := SelectPaperInterval(cp, paperTimingFixture(now, 2), RiskSnapshot{Ready: true, LambdaPerHour: .5}, now); err == nil {
		t.Fatal("scheduler int32 overflow accepted")
	}
}
