package policy

import (
	"math"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func TestExplicitHourlyProtectionBoundsLowRiskButRemainsAdaptive(t *testing.T) {
	now := time.Now().UTC()
	cp := CheckpointPolicy{MinIntervalSeconds: 60, MaxIntervalSeconds: 3600, MeasuredCosts: MeasuredCosts{CheckpointSeconds: 138, CopySeconds: 57, ObservedAt: now.Format(time.RFC3339)}}
	for _, tc := range []struct {
		lambda float64
		spot   int64
		want   int64
	}{
		{.0009212361959754303, 3, 3600}, {0, 3, 3600}, {.3458397557619131, 0, 3600},
	} {
		seconds, _, _ := CheckpointIntervalDecision(cp, RiskSnapshot{Ready: true, LambdaPerHour: tc.lambda}, RuntimeSnapshot{}, tc.spot, now)
		if seconds != tc.want {
			t.Fatalf("lambda=%g spot=%d selected=%d", tc.lambda, tc.spot, seconds)
		}
	}
	seconds, evaluated, _ := CheckpointIntervalDecision(cp, RiskSnapshot{Ready: true, LambdaPerHour: .3458397557619131}, RuntimeSnapshot{}, 3, now)
	if !evaluated || seconds <= 60 || seconds >= 3600 {
		t.Fatalf("higher hazard must still optimize inside bounds: %d %v", seconds, evaluated)
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

func TestPausedScheduleRetainsSelectedCostFreshnessAfterRestart(t *testing.T) {
	now := time.Date(2026, 11, 19, 5, 0, 0, 0, time.UTC)
	obj := NewObject("NodeRequest")
	obj.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{
		"checkpointIntervalSeconds": int64(0), "reason": "group_intent_quiesced",
		"lastSelectedIntervalSeconds": int64(18000),
	}}
	cp := ReadPolicyInput(obj).Checkpoint
	if cp.LastIntervalSeconds != 18000 {
		t.Fatal("pause forgot the previously selected interval")
	}
	cp.MeasuredCosts = MeasuredCosts{CheckpointSeconds: 120, CopySeconds: 60, ObservedAt: now.Add(-30 * time.Minute).Format(time.RFC3339)}
	if _, ok := AdaptiveCheckpointInterval(cp, RiskSnapshot{Ready: true, LambdaPerHour: .001}, RuntimeSnapshot{}, 3, now); !ok {
		t.Fatal("pause/restart caused premature cost expiry")
	}
	// Compatibility with previously deployed statuses before the persistent field.
	unstructured.RemoveNestedField(obj.Object, "status", "checkpoint", "lastSelectedIntervalSeconds")
	_ = unstructured.SetNestedField(obj.Object, int64(600), "status", "checkpoint", "checkpointIntervalSeconds")
	if got := ReadPolicyInput(obj).Checkpoint.LastIntervalSeconds; got != 600 {
		t.Fatalf("legacy status interval lost: %d", got)
	}
}
