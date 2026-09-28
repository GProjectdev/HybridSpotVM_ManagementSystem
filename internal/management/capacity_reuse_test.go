package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/types"
	"testing"
	"time"
)

func TestCapacityReusesOnlyBoundCandidate(t *testing.T) {
	for _, variant := range []string{"valid", "wrong-source-uid", "wrong-operation", "wrong-role", "wrong-market", "disabled", "extra"} {
		t.Run(variant, func(t *testing.T) {
			now := mustParseTime(t, "2026-09-26T00:00:00Z")
			policy := policyFixtureForCapacity(now, 1)
			policy.Object["spec"].(map[string]interface{})["replacement"] = map[string]interface{}{"enabled": variant != "disabled"}
			input := p.ReadPolicySpec(policy)
			old := p.NewNodeProvision(input, 0, "Spot")
			old.SetUID(types.UID("source-full-uid"))
			candidate := old.DeepCopy()
			candidate.SetName(replacementNodeProvisionName(old.GetName(), string(old.GetUID())))
			candidate.SetUID(types.UID("candidate-uid"))
			labels := candidate.GetLabels()
			labels[p.LabelRole] = "replacement"
			candidate.SetLabels(labels)
			candidate.Object["spec"].(map[string]interface{})["marketType"] = "OnDemand"
			annotations := map[string]string{
				"training.dcnlab.com/replaces-nodeprovision":     old.GetName(),
				"training.dcnlab.com/replaces-nodeprovision-uid": string(old.GetUID()),
				"training.dcnlab.com/recovery-operation":         replacementOperationName(old.GetName(), string(old.GetUID())),
			}
			switch variant {
			case "wrong-source-uid":
				annotations["training.dcnlab.com/replaces-nodeprovision-uid"] = "stale"
			case "wrong-operation":
				annotations["training.dcnlab.com/recovery-operation"] = "stale"
			case "wrong-role":
				labels[p.LabelRole] = "worker"
				candidate.SetLabels(labels)
			case "wrong-market":
				candidate.Object["spec"].(map[string]interface{})["marketType"] = "Spot"
			}
			candidate.SetAnnotations(annotations)
			r := policyReconcilerFixture(t, func() time.Time { return now }, policy, old, candidate)
			if variant == "extra" {
				extra := candidate.DeepCopy()
				extra.SetName("unrelated-extra")
				extra.SetResourceVersion("")
				if err := r.Create(context.Background(), extra); err != nil {
					t.Fatal(err)
				}
			}
			result, err := r.checkCapacityLifecycle(context.Background(), policy, input, p.Decision{DesiredWorkers: 1, OnDemandWorkers: 1})
			if err != nil {
				t.Fatal(err)
			}
			if variant == "valid" {
				if !result.ReplacementRequired || result.Reason == "capacity_overshoot" {
					t.Fatalf("bound candidate blocked: %#v", result)
				}
			} else if result.Reason != "capacity_overshoot" {
				t.Fatalf("unsafe candidate accepted: %#v", result)
			}
		})
	}
}
