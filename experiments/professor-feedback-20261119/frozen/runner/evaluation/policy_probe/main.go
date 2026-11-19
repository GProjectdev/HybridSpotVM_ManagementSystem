// Offline diagnostic of the existing policy implementation; never calls a cluster.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func main() {
	config := flag.String("config", "", "reviewed final run config JSON")
	flag.Parse()
	body, err := os.ReadFile(*config)
	if err != nil {
		panic(err)
	}
	var c map[string]interface{}
	if err := json.Unmarshal(body, &c); err != nil {
		panic(err)
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"targetWorkers": int64(2), "policy": c["existing_policy"], "checkpoint": c["existing_checkpoint"]},
	}}
	input := p.ReadPolicyInput(obj)
	// JSON numeric conversion is handled by ReadPolicyInput, as for API objects.
	if input.TargetWorkers != 2 {
		panic("two-worker fixture required")
	}
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	rt := p.RuntimeSnapshot{WorldSize: 2, ReadyRanks: 2}
	rows := []map[string]interface{}{}
	for _, fixed := range []bool{true, false} {
		candidate := input
		one := int64(1)
		if fixed {
			candidate.FixedOnDemand = &one
		} else {
			candidate.FixedOnDemand = nil
		}
		for _, lambda := range []float64{.01, .02, .05, .050001, .10, .20, .22314355131420976, .223144, .3702637197335419, .8} {
			d := p.DecideAt(candidate, rt, p.RiskSnapshot{Ready: true, LambdaPerHour: lambda}, now)
			rows = append(rows, map[string]interface{}{"fixed_on_demand_one": fixed, "lambda_per_hour": lambda, "decision": d})
		}
	}
	result := map[string]interface{}{"kind": "OFFLINE_COUNTERFACTUAL_NOT_GPU_MEASUREMENT", "config": *config,
		"implementation": "internal/policy/contract.go:DecideAt", "runtime_fixture": "two ready ranks; no measured costs/async profile/prices/economics", "rows": rows}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
}
