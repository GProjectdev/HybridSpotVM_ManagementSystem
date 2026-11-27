package framework

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

func TestGroupRiskSchemaAdmissionAndRoundTrip(t *testing.T) {
	data, err := os.ReadFile("../../config/framework/trainingruns.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd extv1.CustomResourceDefinition
	if err = yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	schema := &ext.JSONSchemaProps{}
	if err = extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, schema, nil); err != nil {
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
	for _, tc := range []struct {
		name, method, basis string
		q                   float64
		group, valid        bool
	}{
		{"legacy-omitted", "", "", 0, false, true},
		{"paper", "paper-eq6-7", "", 0, false, true},
		{"group", "group-risk-v1", "per-instance-independent", .8, true, true},
		{"certainty", "group-risk-v1", "per-instance-independent", 1, true, true},
		{"missing-group", "group-risk-v1", "", 0, false, false},
		{"wrong-method", "typo", "", 0, false, false},
		{"paper-group", "paper-eq6-7", "per-instance-independent", .8, true, false},
		{"implicit-group", "", "per-instance-independent", .8, true, false},
		{"aggregate-rate", "group-risk-v1", "aggregate", .8, true, false},
		{"zero-q", "group-risk-v1", "per-instance-independent", 0, true, false},
		{"excess-q", "group-risk-v1", "per-instance-independent", 1.1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, _ := initialObjects("Observe")
			policy := map[string]interface{}{"minOnDemand": int64(0), "forecastSeconds": int64(3600)}
			if tc.method != "" {
				policy["method"] = tc.method
			}
			if tc.group {
				policy["groupRisk"] = map[string]interface{}{"minimumSurvivalProbability": tc.q, "hazardBasis": tc.basis}
			}
			run.Object["spec"].(map[string]interface{})["resourcePolicy"] = policy
			errs := validation.ValidateCustomResource(field.NewPath("run"), run.Object, validator)
			celErrs, _ := cel.Validate(context.Background(), field.NewPath("run"), ss, run.Object, nil, celconfig.RuntimeCELCostBudget)
			if (len(errs) == 0 && len(celErrs) == 0) != tc.valid {
				t.Fatal(errs, celErrs)
			}
			if tc.valid {
				pruning.Prune(run.Object, ss, true)
				var s RunSpec
				if err := decodeSpec(run, &s); err != nil {
					t.Fatal(err)
				}
				if err := s.Validate(); err != nil {
					t.Fatal(err)
				}
				if tc.group && (s.ResourcePolicy.GroupRisk == nil || s.ResourcePolicy.GroupRisk.MinimumSurvivalProbability != tc.q || s.ResourcePolicy.GroupRisk.HazardBasis != tc.basis) {
					t.Fatal("group contract pruned", s)
				}
			}
		})
	}
}
