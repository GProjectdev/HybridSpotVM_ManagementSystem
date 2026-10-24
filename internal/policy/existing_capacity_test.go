package policy

import (
	"context"
	"os"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structural "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func existingCapacityPolicyFixture() *unstructured.Unstructured {
	o := NewObject("TrainingPolicy")
	o.Object["spec"] = map[string]interface{}{
		"sourceCluster": "onpre1", "targetWorkers": int64(1),
		"workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload-uid"},
		"capacity":    map[string]interface{}{"mode": "Existing", "existingNodes": []interface{}{map[string]interface{}{"rank": int64(0), "nodeName": "aws-gpu-demo-worker-01", "nodeUID": "node-uid"}}},
	}
	return o
}

func TestExistingCapacityContractAndAdmission(t *testing.T) {
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
	for _, tc := range []struct {
		name   string
		mutate func(map[string]interface{}, map[string]interface{})
		valid  bool
	}{
		{"existing", func(s, c map[string]interface{}) {}, true},
		{"legacy", func(s, c map[string]interface{}) { delete(s, "capacity") }, true},
		{"provisioned", func(s, c map[string]interface{}) { c["mode"] = "Provisioned"; delete(c, "existingNodes") }, true},
		{"unknown-mode", func(s, c map[string]interface{}) { c["mode"] = "existing" }, false},
		{"empty-mode", func(s, c map[string]interface{}) { c["mode"] = "" }, false},
		{"wrong-mode-type", func(s, c map[string]interface{}) { c["mode"] = true }, false},
		{"missing-opt-in", func(s, c map[string]interface{}) { delete(c, "mode") }, false},
		{"missing-nodes", func(s, c map[string]interface{}) { delete(c, "existingNodes") }, false},
		{"empty-nodes", func(s, c map[string]interface{}) { c["existingNodes"] = []interface{}{} }, false},
		{"wrong-list-type", func(s, c map[string]interface{}) { c["existingNodes"] = "worker" }, false},
		{"missing-rank", func(s, c map[string]interface{}) {
			delete(c["existingNodes"].([]interface{})[0].(map[string]interface{}), "rank")
		}, false},
		{"wrong-rank-type", func(s, c map[string]interface{}) {
			c["existingNodes"].([]interface{})[0].(map[string]interface{})["rank"] = "0"
		}, false},
		{"negative-rank", func(s, c map[string]interface{}) {
			c["existingNodes"].([]interface{})[0].(map[string]interface{})["rank"] = int64(-1)
		}, false},
		{"out-of-range-rank", func(s, c map[string]interface{}) {
			c["existingNodes"].([]interface{})[0].(map[string]interface{})["rank"] = int64(1)
		}, false},
		{"missing-uid", func(s, c map[string]interface{}) {
			delete(c["existingNodes"].([]interface{})[0].(map[string]interface{}), "nodeUID")
		}, false},
		{"empty-uid", func(s, c map[string]interface{}) {
			c["existingNodes"].([]interface{})[0].(map[string]interface{})["nodeUID"] = ""
		}, false},
		{"whitespace-uid", func(s, c map[string]interface{}) {
			c["existingNodes"].([]interface{})[0].(map[string]interface{})["nodeUID"] = " uid "
		}, false},
		{"invalid-name", func(s, c map[string]interface{}) {
			c["existingNodes"].([]interface{})[0].(map[string]interface{})["nodeName"] = "Bad/Node"
		}, false},
		{"incomplete-world", func(s, c map[string]interface{}) { s["targetWorkers"] = int64(2) }, false},
		{"spot-composition", func(s, c map[string]interface{}) { s["policy"] = map[string]interface{}{"fixedOnDemand": int64(0)} }, false},
		{"two-ranks", func(s, c map[string]interface{}) {
			s["targetWorkers"] = int64(2)
			c["existingNodes"] = append(c["existingNodes"].([]interface{}), map[string]interface{}{"rank": int64(1), "nodeName": "worker-2", "nodeUID": "uid-2"})
		}, true},
		{"duplicate-node", func(s, c map[string]interface{}) {
			s["targetWorkers"] = int64(2)
			c["existingNodes"] = append(c["existingNodes"].([]interface{}), map[string]interface{}{"rank": int64(1), "nodeName": "aws-gpu-demo-worker-01", "nodeUID": "uid-2"})
		}, false},
		{"duplicate-rank", func(s, c map[string]interface{}) {
			s["targetWorkers"] = int64(2)
			c["existingNodes"] = append(c["existingNodes"].([]interface{}), map[string]interface{}{"rank": int64(0), "nodeName": "worker-2", "nodeUID": "uid-2"})
		}, false},
		{"duplicate-uid", func(s, c map[string]interface{}) {
			s["targetWorkers"] = int64(2)
			c["existingNodes"] = append(c["existingNodes"].([]interface{}), map[string]interface{}{"rank": int64(1), "nodeName": "worker-2", "nodeUID": "node-uid"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := existingCapacityPolicyFixture()
			s := o.Object["spec"].(map[string]interface{})
			c := s["capacity"].(map[string]interface{})
			tc.mutate(s, c)
			if err := ValidateCapacity(ReadPolicyInput(o)); (err == nil) != tc.valid {
				t.Fatalf("runtime valid=%v: %v", tc.valid, err)
			}
			errs := validation.ValidateCustomResource(field.NewPath("policy"), o.Object, validator)
			if len(errs) == 0 {
				celErrs, _ := cel.Validate(context.Background(), field.NewPath("policy"), ss, o.Object, nil, celconfig.RuntimeCELCostBudget)
				errs = append(errs, celErrs...)
			}
			if (len(errs) == 0) != tc.valid {
				t.Fatalf("admission valid=%v: %v", tc.valid, errs.ToAggregate())
			}
			if tc.valid {
				pruning.Prune(o.Object, ss, true)
				if err := ValidateCapacity(ReadPolicyInput(o)); err != nil {
					t.Fatalf("pruned contract: %v", err)
				}
			}
		})
	}
}

func TestExistingCapacityAdmissionTransitions(t *testing.T) {
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
	ss, err := structural.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	cel := schemacel.NewValidator(ss, true, celconfig.PerCallLimit)
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured, *unstructured.Unstructured)
		valid  bool
	}{
		{"unchanged", func(old, next *unstructured.Unstructured) {}, true},
		{"checkpoint-only", func(old, next *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(next.Object, int64(120), "spec", "checkpoint", "minIntervalSeconds")
		}, true},
		{"existing-to-provisioned", func(old, next *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(next.Object, "Provisioned", "spec", "capacity", "mode")
			unstructured.RemoveNestedField(next.Object, "spec", "capacity", "existingNodes")
		}, false},
		{"remove-capacity", func(old, next *unstructured.Unstructured) {
			unstructured.RemoveNestedField(next.Object, "spec", "capacity")
		}, false},
		{"change-node-uid", func(old, next *unstructured.Unstructured) {
			nodes, _, _ := unstructured.NestedSlice(next.Object, "spec", "capacity", "existingNodes")
			nodes[0].(map[string]interface{})["nodeUID"] = "new-uid"
			_ = unstructured.SetNestedSlice(next.Object, nodes, "spec", "capacity", "existingNodes")
		}, false},
		{"change-node-name", func(old, next *unstructured.Unstructured) {
			nodes, _, _ := unstructured.NestedSlice(next.Object, "spec", "capacity", "existingNodes")
			nodes[0].(map[string]interface{})["nodeName"] = "new-node"
			_ = unstructured.SetNestedSlice(next.Object, nodes, "spec", "capacity", "existingNodes")
		}, false},
		{"provisioned-to-existing", func(old, next *unstructured.Unstructured) {
			unstructured.RemoveNestedField(old.Object, "spec", "capacity")
		}, false},
		{"legacy-explicit-default", func(old, next *unstructured.Unstructured) {
			unstructured.RemoveNestedField(old.Object, "spec", "capacity")
			unstructured.RemoveNestedField(next.Object, "spec", "capacity")
			_ = unstructured.SetNestedField(next.Object, "Provisioned", "spec", "capacity", "mode")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := existingCapacityPolicyFixture()
			next := old.DeepCopy()
			tc.mutate(old, next)
			errs, _ := cel.Validate(context.Background(), field.NewPath("policy"), ss, next.Object, old.Object, celconfig.RuntimeCELCostBudget)
			if (len(errs) == 0) != tc.valid {
				t.Fatalf("transition valid=%v: %v", tc.valid, errs.ToAggregate())
			}
		})
	}
}

func TestExistingCapacityCRDIsAdmissible(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/trainingpolicies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd extv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	extv1.SetDefaults_CustomResourceDefinition(&crd)
	crd.Status.StoredVersions = []string{"v1alpha1"}
	internal := &ext.CustomResourceDefinition{}
	if err := extv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, internal, nil); err != nil {
		t.Fatal(err)
	}
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), internal); len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
}

func TestExistingCapacityDoesNotAlterCheckpointDefaults(t *testing.T) {
	o := existingCapacityPolicyFixture()
	existing := ReadPolicyInput(o)
	unstructured.RemoveNestedField(o.Object, "spec", "capacity")
	legacy := ReadPolicyInput(o)
	if legacy.Capacity.Mode != CapacityModeProvisioned || !legacy.Checkpoint.Resume || existing.Checkpoint.Resume != legacy.Checkpoint.Resume || existing.Checkpoint.MinIntervalSeconds != legacy.Checkpoint.MinIntervalSeconds || existing.Checkpoint.MaxIntervalSeconds != legacy.Checkpoint.MaxIntervalSeconds {
		t.Fatal("capacity changed ordinary checkpoint defaults")
	}
}
