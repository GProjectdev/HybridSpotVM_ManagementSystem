package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

func TestEmergencySameMarketRequiresBoundEventAndPreservesPlannedTransitions(t *testing.T) {
	for _, tc := range []struct {
		old, desired, event string
		valid               bool
	}{
		{"Spot", "Spot", "notice", true}, {"Spot", "OnDemand", "notice", true},
		{"Spot", "Spot", "", false}, {"OnDemand", "OnDemand", "", false},
		{"OnDemand", "OnDemand", "notice", false}, {"OnDemand", "Spot", "notice", false},
		{"Spot", "OnDemand", "", true}, {"OnDemand", "Spot", "", true},
		{"Spot", "bogus", "notice", false},
	} {
		if got := validReplacementTransition(tc.old, tc.desired, tc.event); got != tc.valid {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
	old := replacementOldNodeProvisionFixture()
	op := replacementOperationFixture()
	op.SetName(replacementOperationName(old.GetName(), string(old.GetUID())))
	_ = unstructured.SetNestedField(op.Object, op.GetName(), "spec", "operation")
	_ = unstructured.SetNestedField(op.Object, "Spot", "spec", "desiredMarketType")
	op.SetAnnotations(map[string]string{annotationEmergencyEventID: "notice"})
	spec, err := readReplacementSpec(op)
	if err != nil {
		t.Fatal(err)
	}
	if verifyReplacementOldNode(old, spec) == nil {
		t.Fatal("missing event accepted")
	}
	setEmergencyReplacementSpotEvidence(old, "notice")
	if err := verifyReplacementOldNode(old, spec); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(old.Object, "i-foreign", "status", "spot", "instanceID")
	if verifyReplacementOldNode(old, spec) == nil {
		t.Fatal("foreign provider accepted")
	}
}
func TestEmergencySpotReplacementCreatesSpotAndRetainsIndependentUID(t *testing.T) {
	now := mustParseTime(t, "2026-11-22T00:00:00Z")
	old := replacementOldNodeProvisionFixture()
	setEmergencyReplacementSpotEvidence(old, "notice")
	op := replacementOperationFixture()
	op.SetName(replacementOperationName(old.GetName(), string(old.GetUID())))
	_ = unstructured.SetNestedField(op.Object, op.GetName(), "spec", "operation")
	_ = unstructured.SetNestedField(op.Object, "Spot", "spec", "desiredMarketType")
	op.SetAnnotations(map[string]string{annotationEmergencyEventID: "notice"})
	r := replacementReconcilerFixture(t, now, op, replacementPolicyFixture(), old)
	spec, err := readReplacementSpec(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyReplacementOldNode(old, spec); err != nil {
		t.Fatal(err)
	}
	np, created, err := r.ensureReplacementNodeProvision(context.Background(), "default", old, spec)
	if err != nil || !created || stringField(np.Object, "spec", "marketType") != "Spot" {
		t.Fatalf("np=%v created=%v err=%v", np, created, err)
	}
	if stringField(np.Object, "spec", "awsConfig", "keyPairName") != "" {
		t.Fatal("old key inherited")
	}
	got := p.NewObject("NodeProvision")
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(old), got); err != nil {
		t.Fatal("old was prematurely deleted", err)
	}
}
