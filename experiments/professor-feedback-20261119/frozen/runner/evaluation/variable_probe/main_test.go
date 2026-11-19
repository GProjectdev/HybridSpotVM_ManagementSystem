package main

import (
	"encoding/json"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"testing"
)

func TestAdapterPreservesIntegerPolicyFieldsAndProductionStage(t *testing.T) {
	policy := json.RawMessage(`{"apiVersion":"training.dcnlab.com/v1alpha1","kind":"NodeRequest","spec":{"targetWorkers":4,"policy":{"method":"paper-eq6-7","alpha":0.8,"minOnDemand":1,"forecastHorizonSeconds":3600,"economics":{"enabled":true}},"checkpoint":{"minIntervalSeconds":60,"maxIntervalSeconds":600}}}`)
	req := request{ID: "test", Policy: policy, Now: "2026-11-19T00:00:00Z", Runtime: p.RuntimeSnapshot{WorldSize: 4, ReadyRanks: 4}, Risk: p.RiskSnapshot{Ready: true, LambdaPerHour: .1, SpotPricePerHour: .5, OnDemandPricePerHour: 1}}
	d, err := evaluate(req)
	if err != nil || d.DesiredWorkers != 4 || d.OnDemandWorkers != 1 || d.SpotWorkers != 3 || d.CostEvaluated || d.VMPolicyStage != "ThroughputOnlyBootstrap" {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
	req.Risk.LambdaPerHour = .3458397557619131
	d, err = evaluate(req)
	if err != nil || d.OnDemandWorkers != 4 || d.SpotWorkers != 0 {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
}
