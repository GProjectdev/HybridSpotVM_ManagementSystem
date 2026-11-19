package policy

import (
	"math"
	"testing"
	"time"
)

func TestPositiveRiskHasNoDefaultTenMinuteCap(t *testing.T) {
	now := time.Date(2026, 11, 19, 4, 0, 0, 0, time.UTC)
	cp := CheckpointPolicy{MeasuredCosts: MeasuredCosts{CheckpointSeconds: 120, CopySeconds: 60, ObservedAt: now.Format(time.RFC3339)}}
	seconds, evaluated, source := CheckpointIntervalDecision(cp, RiskSnapshot{Ready: true, LambdaPerHour: .001}, RuntimeSnapshot{}, 3, now)
	// Independent continuous optimum sqrt(2*120/(3*.001/3600))
	// is 16970.5627; compare the two adjacent integer objective values.
	if seconds != 16971 || !evaluated || source != "checkpoint-cost-analytic" {
		t.Fatalf("unexpected uncapped optimum: %d %v %s", seconds, evaluated, source)
	}
	cp.MaxIntervalSeconds = 600
	if seconds, evaluated, _ := CheckpointIntervalDecision(cp, RiskSnapshot{Ready: true, LambdaPerHour: .001}, RuntimeSnapshot{}, 3, now); seconds != 600 || !evaluated {
		t.Fatalf("explicit operator maximum lost: %d %v", seconds, evaluated)
	}
}

func TestHourlyFallbackDoesNotRequireInventedCostMeasurements(t *testing.T) {
	for _, paper := range []bool{false, true} {
		for _, tc := range []struct {
			lambda float64
			spot   int64
		}{{0, 3}, {.1, 0}, {0, 0}} {
			seconds, evaluated, source := CheckpointIntervalDecision(CheckpointPolicy{Paper: PaperProfile{Enabled: paper}}, RiskSnapshot{Ready: true, LambdaPerHour: tc.lambda}, RuntimeSnapshot{}, tc.spot, time.Now())
			if seconds != 3600 || evaluated || source != "hourly-risk-refresh-fallback" {
				t.Fatalf("fallback must be hourly and not claim measured optimization: %d %v %s", seconds, evaluated, source)
			}
		}
	}
	for _, risk := range []RiskSnapshot{{}, {Ready: true, LambdaPerHour: -1}, {Ready: true, LambdaPerHour: math.NaN()}, {Ready: true, LambdaPerHour: math.Inf(1)}} {
		if _, _, source := CheckpointIntervalDecision(CheckpointPolicy{}, risk, RuntimeSnapshot{}, 0, time.Now()); source == "hourly-risk-refresh-fallback" {
			t.Fatal("unknown or invalid risk must not become zero risk")
		}
	}
}

func TestUncappedCostFreshnessTracksSelectedIntervalWithoutRetimestamping(t *testing.T) {
	now := time.Now().UTC()
	cp := CheckpointPolicy{LastIntervalSeconds: 16971}
	if age := CheckpointCostMaxAge(cp); age != 17571*time.Second {
		t.Fatalf("freshness used representation ceiling or old 600 seconds: %v", age)
	}
	for _, tc := range []struct {
		age       time.Duration
		evaluated bool
	}{{30 * time.Minute, true}, {17571 * time.Second, true}, {17572 * time.Second, false}, {-time.Second, false}} {
		cp.MeasuredCosts = MeasuredCosts{CheckpointSeconds: 120, CopySeconds: 60, ObservedAt: now.Add(-tc.age).Format(time.RFC3339Nano)}
		original := cp.MeasuredCosts.ObservedAt
		_, evaluated := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true, LambdaPerHour: .001}, RuntimeSnapshot{}, 3, now)
		if evaluated != tc.evaluated || cp.MeasuredCosts.ObservedAt != original {
			t.Fatalf("age=%v evaluated=%v timestamp=%s", tc.age, evaluated, cp.MeasuredCosts.ObservedAt)
		}
	}
}

func TestDefaultBoundsOnlyUseSchedulerRepresentation(t *testing.T) {
	lower, upper, err := checkpointIntervalBounds(CheckpointPolicy{MinIntervalSeconds: 1000})
	if err != nil || lower != 1000 || upper != math.MaxInt32 {
		t.Fatalf("default bounds=%d..%d err=%v", lower, upper, err)
	}
}
