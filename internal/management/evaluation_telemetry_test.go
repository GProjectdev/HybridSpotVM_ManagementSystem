package management

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
)

func TestDecisionTelemetryKeepsVariableProvenanceAndDoesNotMutatePolicy(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	input := p.PolicyInput{TargetWorkers: 4, MinOnDemand: 1, Alpha: .8, VMPolicyMethod: "paper-eq6-7", ForecastSeconds: 3600, Capacity: p.CapacityDefaults{CredentialsName: "DO_NOT_SERIALIZE_CREDENTIAL_REF"}, Checkpoint: p.CheckpointPolicy{MinIntervalSeconds: 60, MaxIntervalSeconds: 600, MeasuredCosts: p.MeasuredCosts{CheckpointSeconds: 195, CopySeconds: 70, ObservedAt: now.Format(time.RFC3339)}}, Economics: p.EconomicsPolicy{Enabled: true, LossCostPerEviction: -1}}
	before := input
	rt := p.RuntimeSnapshot{WorldSize: 4, ReadyRanks: 4, IterationTimeSeconds: .8}
	risk := p.RiskSnapshot{Ready: true, LambdaPerHour: .1, SpotPricePerHour: .5, OnDemandPricePerHour: 1}
	decision := p.DecideAt(input, rt, risk, now)
	pairs := evaluationDecisionFields("checkpoint-coordinator", input, rt, risk, decision, now)
	fields := map[string]interface{}{}
	for i := 0; i < len(pairs); i += 2 {
		fields[pairs[i].(string)] = pairs[i+1]
	}
	if !reflect.DeepEqual(input, before) || fields["checkpointSeconds"] != float64(195) || fields["alpha"] != .8 || fields["intervalSource"] != "checkpoint-cost-analytic" || fields["paperAsyncProfileEnabled"] != false || fields["costEvaluated"] != false || fields["decisionActor"] != "checkpoint-coordinator" {
		t.Fatalf("fields=%v", fields)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "DO_NOT_SERIALIZE") || strings.Contains(string(raw), "Capacity") {
		t.Fatal("telemetry leaked unrelated capacity configuration")
	}
	if p.DecideAt(input, rt, risk, now) != decision {
		t.Fatal("telemetry altered policy decision")
	}
	if fields["checkpointSafetyMaximumSeconds"] != int64(3600) || fields["checkpointSafetyMaximumReached"] != false {
		t.Fatal("policy safety bound provenance missing")
	}
}
