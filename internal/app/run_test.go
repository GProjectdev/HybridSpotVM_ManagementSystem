package app

import "testing"

func TestIndependentLeaders(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range []string{"vm-spot-risk-collector", "policy-manager", "checkpoint-coordinator", "spot-recovery-controller", "training-runtime-collector"} {
		id := leaderElectionID(name)
		if seen[id] || id == "hybridspot-management" || id == "hybridspot-runtime" {
			t.Fatalf("component %s shares another or legacy leader lease: %s", name, id)
		}
		seen[id] = true
	}
}
