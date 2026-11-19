// Offline JSON-lines adapter to the unchanged production policy, not a new model.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
)

type request struct {
	ID      string            `json:"id"`
	Policy  json.RawMessage   `json:"policy"`
	Runtime p.RuntimeSnapshot `json:"runtime"`
	Risk    p.RiskSnapshot    `json:"risk"`
	Now     string            `json:"now"`
}

func evaluate(req request) (p.Decision, error) {
	obj := p.NewObject("NodeRequest")
	if err := obj.UnmarshalJSON(req.Policy); err != nil {
		return p.Decision{}, err
	}
	input := p.ReadPolicyInput(obj)
	if req.ID == "" || (input.TargetWorkers != 2 && input.TargetWorkers != 4) || input.VMPolicyMethod != "paper-eq6-7" {
		return p.Decision{}, fmt.Errorf("explicit2/4-rank existing paper-eq6-7 input required")
	}
	if err := p.ValidateFixedComposition(input); err != nil {
		return p.Decision{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, req.Now)
	if err != nil {
		return p.Decision{}, err
	}
	return p.DecideAt(input, req.Runtime, req.Risk, at), nil
}

func main() {
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 2*1024*1024)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var req request
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		decision, err := evaluate(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := enc.Encode(map[string]interface{}{"id": req.ID, "decision": decision, "kind": "OFFLINE_UNCHANGED_PRODUCTION_DECIDEAT_NOT_GPU_MEASUREMENT"}); err != nil {
			panic(err)
		}
	}
	if err := scan.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
