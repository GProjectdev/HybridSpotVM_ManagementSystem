package policy

import (
	"context"
	"os"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structural "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestFixedCompositionAdmission(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/trainingpolicies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd extv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	schema := &ext.JSONSchemaProps{}
	if err := extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, schema, nil); err != nil {
		t.Fatal(err)
	}
	validator, _, err := validation.NewSchemaValidator(schema)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := structural.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	cel := schemacel.NewValidator(ss, true, celconfig.PerCallLimit)
	if cel == nil {
		t.Fatal("missing composition CEL validation")
	}
	for _, tc := range []struct {
		name   string
		policy map[string]interface{}
		valid  bool
	}{
		{"legacy-adaptive", map[string]interface{}{"minOnDemand": int64(1)}, true},
		{"fixed-one", map[string]interface{}{"fixedOnDemand": int64(1)}, true},
		{"matching-floor", map[string]interface{}{"fixedOnDemand": int64(1), "minOnDemand": int64(1)}, true},
		{"explicit-all-spot", map[string]interface{}{"fixedOnDemand": int64(0)}, true},
		{"explicit-all-od", map[string]interface{}{"fixedOnDemand": int64(2)}, true},
		{"negative", map[string]interface{}{"fixedOnDemand": int64(-1)}, false},
		{"exceeds-target", map[string]interface{}{"fixedOnDemand": int64(3)}, false},
		{"below-floor", map[string]interface{}{"fixedOnDemand": int64(0), "minOnDemand": int64(1)}, false},
		{"wrong-type", map[string]interface{}{"fixedOnDemand": "1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := NewObject("TrainingPolicy")
			obj.SetName("evaluation2")
			obj.SetNamespace("default")
			obj.Object["spec"] = map[string]interface{}{
				"workloadRef":   map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload-uid"},
				"sourceCluster": "aws", "targetWorkers": int64(2), "policy": tc.policy,
			}
			errs := validation.ValidateCustomResource(field.NewPath("policy"), obj.Object, validator)
			if len(errs) == 0 {
				celErrs, _ := cel.Validate(context.Background(), field.NewPath("policy"), ss, obj.Object, nil, celconfig.RuntimeCELCostBudget)
				errs = append(errs, celErrs...)
			}
			if (len(errs) == 0) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, errs.ToAggregate())
			}
			if tc.valid {
				value, present := tc.policy["fixedOnDemand"]
				pruning.Prune(obj.Object, ss, true)
				input := ReadPolicyInput(obj)
				if present && (input.FixedOnDemand == nil || *input.FixedOnDemand != value.(int64)) {
					t.Fatal("admission pruned fixed composition")
				}
			}
		})
	}
}
