package policy

import (
	"math"
	"testing"
	"time"
)

func TestPaperEquationsAndMemoryConstraint(t *testing.T) {
	p := PaperProfile{Asynchronous: true, GPUToDRAMSeconds: 4, StorageSeconds: 12, CheckpointGiB: 2, BufferGiB: 4}
	// T/Ndp=2, Ndp=2, lambda_iter=.5*4/3600, f=4.
	cost, err := PaperCheckpointCost(4, 2, 2, .5, p)
	want := 2.0 + 4.0/4 + (12.0/4 - 2) + (.5*4/3600)*4*2/2
	if err != nil || math.Abs(cost-want) > 1e-12 {
		t.Fatalf("Eq.3 got %v want %v: %v", cost, want, err)
	}
	if _, err := PaperCheckpointCost(2, 2, 2, .5, p); err == nil {
		t.Fatal("Eq.4 violated")
	}
	p.Asynchronous = false
	if _, err := PaperCheckpointCost(4, 2, 2, .5, p); err == nil {
		t.Fatal("sync CRIU treated as async DRAM buffering")
	}
}

func TestPaperSelectionZeroRiskAndMissingEvidence(t *testing.T) {
	now := time.Now().UTC()
	cp := CheckpointPolicy{MaxIntervalSeconds: 80, Paper: PaperProfile{Enabled: true, Asynchronous: true, GPUToDRAMSeconds: 4, StorageSeconds: 2, CheckpointGiB: 1, BufferGiB: 2, ObservedAt: now.Format(time.RFC3339Nano), CandidateIterations: []int64{10, 20}}}
	rt := RuntimeSnapshot{WorldSize: 2, ReadyRanks: 2, GlobalStep: 40, IterationTimeSeconds: 2, IterationMethod: "optimizer-update-window-v1", IterationAggregation: "max-rank-mean", IterationRankCount: 2, IterationStartStep: 20, IterationEndStep: 40, IterationSamples: 20, IterationObservedAt: now.Format(time.RFC3339Nano)}
	best, err := SelectPaperInterval(cp, rt, RiskSnapshot{Ready: true}, now)
	if err != nil || best.Iterations != 40 || best.Seconds != 80 {
		t.Fatalf("zero risk: %+v %v", best, err)
	}
	if seconds, evaluated := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true}, rt, 2, now); seconds != 80 || !evaluated {
		t.Fatalf("measured paper interval: %d %v", seconds, evaluated)
	}
	rt.IterationRankCount = 1
	if _, err := SelectPaperInterval(cp, rt, RiskSnapshot{Ready: true}, now); err == nil {
		t.Fatal("partial timing accepted")
	}
}

func TestPaperBootstrapWithoutMeasurements(t *testing.T) {
	cp := CheckpointPolicy{MinIntervalSeconds: 60, MaxIntervalSeconds: 600, Paper: PaperProfile{Enabled: true}}
	seconds, evaluated := AdaptiveCheckpointInterval(cp, RiskSnapshot{}, RuntimeSnapshot{}, 0, time.Now())
	if seconds < 60 || seconds > 600 || evaluated {
		t.Fatalf("bootstrap must proceed without claiming paper evaluation: %d %v", seconds, evaluated)
	}
}
