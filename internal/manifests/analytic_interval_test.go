package manifests_test

import (
	"context"
	"math"
	"os"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structural "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestAnalyticIntervalTrainingPolicyAdmission(t *testing.T) {
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
	structuralSchema, err := structural.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	celValidator := schemacel.NewValidator(structuralSchema, true, celconfig.PerCallLimit)
	if celValidator == nil {
		t.Fatal("TrainingPolicy CEL validator missing")
	}
	for _, tc := range []struct {
		name      string
		mutate    func(map[string]interface{})
		wantField string
	}{
		{name: "paper-profile-without-candidates"},
		{name: "legacy-candidates-accepted", mutate: func(cp map[string]interface{}) {
			cp["paperProfile"].(map[string]interface{})["candidateIterations"] = []interface{}{int64(10), int64(20)}
			cp["candidateIntervalSeconds"] = []interface{}{int64(60), int64(120)}
		}},
		{name: "max-int32-boundary-accepted", mutate: func(cp map[string]interface{}) {
			cp["minIntervalSeconds"], cp["maxIntervalSeconds"] = int64(math.MaxInt32), int64(math.MaxInt32)
		}},
		{name: "min-exceeds-int32", mutate: func(cp map[string]interface{}) {
			cp["minIntervalSeconds"] = int64(math.MaxInt32) + 1
		}, wantField: "policy.spec.checkpoint.minIntervalSeconds"},
		{name: "max-exceeds-int32", mutate: func(cp map[string]interface{}) {
			cp["maxIntervalSeconds"] = int64(math.MaxInt32) + 1
		}, wantField: "policy.spec.checkpoint.maxIntervalSeconds"},
		{name: "zero-min-rejected", mutate: func(cp map[string]interface{}) {
			cp["minIntervalSeconds"] = int64(0)
		}, wantField: "policy.spec.checkpoint.minIntervalSeconds"},
		{name: "zero-max-rejected", mutate: func(cp map[string]interface{}) {
			cp["maxIntervalSeconds"] = int64(0)
		}, wantField: "policy.spec.checkpoint.maxIntervalSeconds"},
		{name: "inverted-bounds-rejected-by-cel", mutate: func(cp map[string]interface{}) {
			cp["minIntervalSeconds"], cp["maxIntervalSeconds"] = int64(120), int64(60)
		}, wantField: "policy.spec"},
		{name: "required-measurement-still-required", mutate: func(cp map[string]interface{}) {
			delete(cp["paperProfile"].(map[string]interface{}), "storageSeconds")
		}, wantField: "policy.spec.checkpoint.paperProfile.storageSeconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := map[string]interface{}{"paperProfile": map[string]interface{}{
				"enabled": true, "asynchronous": true, "gpuToDramSeconds": float64(2), "storageSeconds": float64(10),
				"checkpointGiB": float64(1), "bufferGiB": float64(2), "observedAt": "2026-09-26T00:03:30Z",
			}}
			if tc.mutate != nil {
				tc.mutate(checkpoint)
			}
			obj := map[string]interface{}{
				"apiVersion": "training.dcnlab.com/v1alpha1", "kind": "TrainingPolicy",
				"metadata": map[string]interface{}{"name": "trainer", "namespace": "default"},
				"spec": map[string]interface{}{
					"workloadRef":   map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload-uid"},
					"sourceCluster": "source", "targetWorkers": int64(1), "checkpoint": checkpoint,
				},
			}
			errs := validation.ValidateCustomResource(field.NewPath("policy"), obj, validator)
			celErrs, _ := celValidator.Validate(context.Background(), field.NewPath("policy"), structuralSchema, obj, nil, celconfig.RuntimeCELCostBudget)
			errs = append(errs, celErrs...)
			if tc.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("valid TrainingPolicy rejected: %v", errs.ToAggregate())
				}
				return
			}
			for _, err := range errs {
				if err.Field == tc.wantField {
					return
				}
			}
			t.Fatalf("expected rejection at %s, got %v", tc.wantField, errs.ToAggregate())
		})
	}
}
