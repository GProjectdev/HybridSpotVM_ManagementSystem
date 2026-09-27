package management

import (
	"context"
	"reflect"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPlacementAdmissionDefaults(t *testing.T) {
	input := p.PolicyInput{Namespace: "fluidcr-demo", PolicyName: "trainer", PolicyUID: "policy-uid"}
	for _, kind := range []string{"TrainingRuntime", "NodeProvision", "FluidCRMigration"} {
		t.Run(kind, func(t *testing.T) {
			obj := p.NewObject(kind)
			obj.SetName("trainer-object")
			obj.SetNamespace(input.Namespace)
			desired := p.NewPropagationPolicyFor(input, obj, "aws")
			existing := desired.DeepCopy()
			spec := existing.Object["spec"].(map[string]interface{})
			spec["conflictResolution"] = "Abort"
			spec["preemption"] = "Never"
			spec["priority"] = int64(0)
			spec["schedulerName"] = "default-scheduler"
			spec["resourceSelectors"].([]interface{})[0].(map[string]interface{})["namespace"] = input.Namespace
			before := desired.DeepCopy()
			r := &PolicyReconciler{Client: fake.NewClientBuilder().WithRuntimeObjects(existing).Build()}
			if err := r.createIfMissing(context.Background(), desired); err != nil {
				t.Fatalf("admission defaults blocked reconciliation: %v", err)
			}
			if !reflect.DeepEqual(before.Object, desired.Object) {
				t.Fatal("comparison mutated desired policy")
			}
			for _, field := range []string{"conflictResolution", "preemption", "schedulerName", "priority", "namespace", "cluster", "selector", "extra"} {
				t.Run(field, func(t *testing.T) {
					changed := existing.DeepCopy()
					s := changed.Object["spec"].(map[string]interface{})
					switch field {
					case "priority":
						s[field] = int64(1)
					case "namespace", "selector":
						key := field
						if field == "selector" {
							key = "name"
						}
						s["resourceSelectors"].([]interface{})[0].(map[string]interface{})[key] = "other"
					case "cluster":
						_ = unstructured.SetNestedStringSlice(changed.Object, []string{"onpre1"}, "spec", "placement", "clusterAffinity", "clusterNames")
					default:
						s[field] = "other"
					}
					if samePlacementSpec(changed, desired) {
						t.Fatal("real drift accepted")
					}
				})
			}
		})
	}
}
