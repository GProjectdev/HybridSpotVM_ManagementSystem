// Offline scan of frozen forecasts through the existing policy. No AWS/cluster calls.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
)

func main() {
	forecast := flag.String("forecasts", "", "existing frozen forecast JSON")
	prices := flag.String("environment", "", "read-only environment receipt with execution prices")
	workers := flag.Int64("workers", 2, "scenario world size: S1 v2=4, S2=2")
	controlOD := flag.Int64("control-od", 1, "fixed-control On-Demand count; v3 A=world size")
	flag.Parse()
	if *workers != 2 && *workers != 4 {
		panic("reviewed scenario world must be 2 or 4")
	}
	if *controlOD < 1 || *controlOD > *workers {
		panic("fixed-control count must be between minOD=1 and world size")
	}
	var data struct {
		Forecasts []struct {
			Hour   int     `json:"evaluation_hour"`
			Lambda float64 `json:"lambda_per_hour"`
		} `json:"forecasts"`
	}
	var env map[string]json.RawMessage
	read := func(path string, into interface{}) {
		b, e := os.ReadFile(path)
		if e != nil {
			panic(e)
		}
		if e = json.Unmarshal(b, into); e != nil {
			panic(e)
		}
	}
	read(*forecast, &data)
	read(*prices, &env)
	if len(data.Forecasts) != 168 {
		panic("complete held-out forecast required")
	}
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	out := map[string]interface{}{"kind": "OFFLINE_EXISTING_DECIDEAT_NOT_LIVE_REPLACEMENT", "economics": "enabled; no fabricated loss observation; throughput bootstrap only", "checkpoint_bounds_proposal_seconds": []int{60, 600}, "rows": []interface{}{}}
	out["world_size"] = *workers
	out["fixed_control_on_demand"] = *controlOD
	out["checkpoint_execution_not_probed"] = true
	rows := []interface{}{}
	for _, typ := range []string{"g5.xlarge", "g6.xlarge"} {
		var price struct {
			Spot float64 `json:"spotPricePerHour"`
			OD   float64 `json:"onDemandPricePerHour"`
		}
		if err := json.Unmarshal(env[typ+"_price"], &price); err != nil || price.Spot <= 0 || price.OD <= 0 {
			panic("verified execution prices required")
		}
		for i, f := range data.Forecasts {
			if f.Hour != i {
				panic("ordered contiguous hours required")
			}
			in := p.PolicyInput{VMPolicyMethod: "paper-eq6-7", TargetWorkers: *workers, MinOnDemand: 1, Alpha: .8, ForecastSeconds: 3600, Economics: p.EconomicsPolicy{Enabled: true, LossCostPerEviction: -1, OnDemandDurationSeconds: 3600}, Checkpoint: p.CheckpointPolicy{MinIntervalSeconds: 60, MaxIntervalSeconds: 600, Resume: true}}
			risk := p.RiskSnapshot{Ready: true, LambdaPerHour: f.Lambda, SpotPricePerHour: price.Spot, OnDemandPricePerHour: price.OD}
			runtime := p.RuntimeSnapshot{WorldSize: *workers, ReadyRanks: *workers}
			adaptive := p.DecideAt(in, runtime, risk, now)
			// Historical 20s/10s acceptance costs, assumed fresh for a counterfactual.
			// This is not a new measurement or the current runtime's cost profile.
			measuredInput := in
			measuredInput.Checkpoint.MeasuredCosts = p.MeasuredCosts{CheckpointSeconds: 20, CopySeconds: 10, ObservedAt: now.Format(time.RFC3339)}
			var measured interface{}
			if *workers == 2 {
				measured = p.DecideAt(measuredInput, runtime, risk, now)
			}
			in.FixedOnDemand = controlOD
			// P0 must also fix checkpoint bounds; fixedOnDemand alone does not.
			in.Checkpoint.MinIntervalSeconds = 600
			in.Checkpoint.MaxIntervalSeconds = 600
			fixed := p.DecideAt(in, runtime, risk, now)
			rows = append(rows, map[string]interface{}{"instance_type": typ, "hour": i, "lambda_per_hour": f.Lambda, "p1": adaptive, "p1_counterfactual_fresh_historical_cost_20_10": measured, "p0_f0_candidate_600": fixed})
		}
	}
	out["rows"] = rows
	b, e := json.MarshalIndent(out, "", "  ")
	if e != nil {
		panic(e)
	}
	fmt.Println(string(b))
}
