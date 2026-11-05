package policy

import (
	"math"
	"testing"
	"time"
)

func TestMeasuredCostMaxAge(t *testing.T) {
	for _, tc := range []struct {
		max  int64
		want time.Duration
	}{
		{0, 10 * time.Minute}, {-1, 10 * time.Minute}, {3600, 70 * time.Minute},
		{math.MaxInt32, (time.Duration(math.MaxInt32) + 600) * time.Second},
		{math.MaxInt32 + 1, 10 * time.Minute}, {math.MaxInt64, 10 * time.Minute},
	} {
		if got := MeasuredCostMaxAge(tc.max); got != tc.want {
			t.Errorf("max=%d age=%v want %v", tc.max, got, tc.want)
		}
	}
}

func TestMeasuredCostFreshness(t *testing.T) {
	now := time.Date(2026, 9, 26, 2, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		max  int64
		age  time.Duration
		want bool
	}{
		{"long interval", 3600, 3106 * time.Second, true},
		{"boundary", 3600, 4200 * time.Second, true},
		{"expired", 3600, 4201 * time.Second, false},
		{"default boundary", 0, 600 * time.Second, true},
		{"default expired", 0, 601 * time.Second, false},
		{"negative default", -1, 601 * time.Second, false},
		{"future", 3600, -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := CheckpointPolicy{MaxIntervalSeconds: tc.max, MeasuredCosts: MeasuredCosts{CheckpointSeconds: 90, CopySeconds: 30, ObservedAt: now.Add(-tc.age).Format(time.RFC3339)}}
			_, got := AdaptiveCheckpointInterval(checkpoint, RiskSnapshot{Ready: true, LambdaPerHour: .05}, RuntimeSnapshot{}, 2, now)
			if got != tc.want {
				t.Fatalf("evaluated=%v want %v", got, tc.want)
			}
		})
	}
}
