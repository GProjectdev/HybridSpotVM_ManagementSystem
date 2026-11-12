package app

import "testing"

func TestIndependentLeaders(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range []string{"public-cloud-vm-collector", "spot-vm-management-controller", "checkpoint-recovery-coordinator", "training-lifecycle-controller", "training-runtime-controller"} {
		id := leaderElectionID(name)
		if seen[id] || id == "hybridspot-management" || id == "hybridspot-runtime" {
			t.Fatalf("component %s shares another or legacy leader lease: %s", name, id)
		}
		seen[id] = true
	}
}

func TestLegacyAliasesShareCanonicalLease(t *testing.T) {
	for alias, canonical := range map[string]string{
		"vm-spot-risk-collector":     "public-cloud-vm-collector",
		"policy-manager":             "spot-vm-management-controller",
		"checkpoint-coordinator":     "checkpoint-recovery-coordinator",
		"spot-recovery-controller":   "checkpoint-recovery-coordinator",
		"training-runtime-collector": "training-runtime-controller",
	} {
		if leaderElectionID(alias) != leaderElectionID(canonical) {
			t.Errorf("%s has an independent lease", alias)
		}
	}
}
