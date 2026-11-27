// CPU-only policy diagnostic. No AWS/Kubernetes clients or mutations.
package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
)

type row struct {
	InputKind string     `json:"inputKind"`
	Hour      string     `json:"hour,omitempty"`
	World     int64      `json:"world"`
	MinOD     int64      `json:"minOD"`
	Lambda    float64    `json:"lambda"`
	Paper     p.Decision `json:"paper"`
	Group     p.Decision `json:"group"`
}

func main() {
	trace := flag.String("trace", "", "optional existing risk_hourly_alignment.csv (read-only, proxy What-if only)")
	flag.Parse()
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	rows := []row{}
	add := func(kind, hour string, lambda float64) {
		for _, n := range []int64{2, 4, 8} {
			for _, floor := range []int64{0, 1} {
				in := p.PolicyInput{VMPolicyMethod: "paper-eq6-7", TargetWorkers: n, MinOnDemand: floor, Alpha: .8, ForecastSeconds: 3600, Checkpoint: p.CheckpointPolicy{MinIntervalSeconds: 60, MaxIntervalSeconds: 3600}}
				risk := p.RiskSnapshot{Ready: true, LambdaPerHour: lambda, SpotPricePerHour: .6, OnDemandPricePerHour: 1.5}
				paper := p.DecideAt(in, p.RuntimeSnapshot{}, risk, now)
				in.VMPolicyMethod = p.GroupRiskMethod
				in.GroupRisk = &p.GroupRiskPolicy{MinimumSurvivalProbability: .8, HazardBasis: p.IndependentPerInstanceHazard}
				group := p.DecideAt(in, p.RuntimeSnapshot{}, risk, now)
				if paper.ProvisioningBlocked || group.ProvisioningBlocked {
					panic("diagnostic input rejected")
				}
				rows = append(rows, row{kind, hour, n, floor, lambda, paper, group})
			}
		}
	}
	for _, lambda := range []float64{0, .01, .03, .05, .06, .08, .1, .15, .2, .25, .3, 1} {
		add("synthetic-sensitivity", "", lambda)
	}
	sha := ""
	if *trace != "" {
		body, err := os.ReadFile(*trace)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(body)
		sha = hex.EncodeToString(sum[:])
		table, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
		if err != nil || len(table) < 2 {
			panic("invalid trace CSV")
		}
		columns := map[string]int{}
		for i, name := range table[0] {
			columns[name] = i
		}
		forecast, ok := columns["SARIMA_predicted_proxy_per_hour"]
		if !ok {
			panic("missing forecast column")
		}
		hour, ok := columns["held_out_hour"]
		if !ok {
			panic("missing hour column")
		}
		for _, record := range table[1:] {
			lambda, err := strconv.ParseFloat(record[forecast], 64)
			if err != nil {
				panic(err)
			}
			add("trace-proxy-what-if", record[hour], lambda)
		}
	}
	out := map[string]interface{}{"category": "Sensitivity/What-if; not GPU measurements or a cost simulation", "parameters": map[string]interface{}{"q": .8, "paperAlpha": .8, "horizonSeconds": 3600, "spotPriceFixture": .6, "odPriceFixture": 1.5, "measuredLossUsed": false}, "assumptions": "Equal independent per-VM hazards; trace values are only proxy scenarios, not validated Seoul G5 hazard. Instantaneous targets omit hold, supply and transition gates.", "tracePath": *trace, "traceSHA256": sha, "rows": rows}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(body))
}
