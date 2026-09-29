package policy

import (
	"math"
	"time"
)

// Loss is per individual eviction, in the same currency as the hourly VM price.
type EconomicsPolicy struct {
	Source              string
	Enabled             bool
	LossCostPerEviction float64
	ObservedAt          string
	MaxAgeSeconds       int64
}

func economicFallback(input PolicyInput, risk RiskSnapshot, now time.Time) (bool, bool) {
	e := input.Economics
	if !e.Enabled || !risk.Ready || !finite(e.LossCostPerEviction) || e.LossCostPerEviction < 0 || !finite(risk.OnDemandPricePerHour) || risk.OnDemandPricePerHour <= 0 || !finite(risk.LambdaPerHour) || risk.LambdaPerHour < 0 {
		return false, false
	}
	observed, err := time.Parse(time.RFC3339, e.ObservedAt)
	maxAge := e.MaxAgeSeconds
	if maxAge <= 0 {
		maxAge = 600
	}
	if err != nil || observed.After(now) || now.Sub(observed).Seconds() > float64(maxAge) {
		return false, false
	}
	hours := float64(maxInt64(1, input.ForecastSeconds)) / 3600
	probability := -math.Expm1(-risk.LambdaPerHour * hours)
	odCost := risk.OnDemandPricePerHour * hours
	expectedLoss := probability * e.LossCostPerEviction
	if !finite(odCost) || !finite(expectedLoss) {
		return false, false
	}
	return odCost < expectedLoss, true
}
