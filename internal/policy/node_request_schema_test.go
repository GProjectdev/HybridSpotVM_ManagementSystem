package policy

import (
	"context"
	"os"
	"strings"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structural "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestNodeRequestWorkloadBindingAdmission(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/noderequests.yaml")
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
		t.Fatal("missing NodeRequest CEL validator")
	}
	makeObject := func(uid string) map[string]interface{} {
		obj := NewObject("NodeRequest")
		obj.SetName("training")
		obj.SetNamespace("default")
		ref := map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer"}
		if uid != "" {
			ref["uid"] = uid
		}
		obj.Object["spec"] = map[string]interface{}{"workloadRef": ref, "sourceCluster": "aws", "targetWorkers": int64(2)}
		return obj.Object
	}
	for _, tc := range []struct {
		name, oldUID, newUID, changedField, changedValue, celMessage string
		create                                                       bool
	}{
		{name: "create-without-uid", create: true},
		{name: "unbound-update"},
		{name: "bind-first-uid", newUID: "original"},
		{name: "bound-idempotent", oldUID: "original", newUID: "original"},
		{name: "change-bound-uid", oldUID: "original", newUID: "replacement", celMessage: "workload UID cannot change after binding"},
		{name: "remove-bound-uid", oldUID: "original", celMessage: "workload UID cannot change after binding"},
		{name: "change-unbound-name", changedField: "name", changedValue: "other", celMessage: "workload identity is immutable"},
		{name: "change-bound-name", oldUID: "original", newUID: "original", changedField: "name", changedValue: "other", celMessage: "workload identity is immutable"},
		{name: "change-api-version", changedField: "apiVersion", changedValue: "apps/v2", celMessage: "workload identity is immutable"},
		{name: "change-kind", changedField: "kind", changedValue: "Deployment", celMessage: "workload identity is immutable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := makeObject(tc.newUID)
			if tc.changedField != "" {
				obj["spec"].(map[string]interface{})["workloadRef"].(map[string]interface{})[tc.changedField] = tc.changedValue
			}
			var old interface{}
			if !tc.create {
				old = makeObject(tc.oldUID)
			}
			schemaErrs := validation.ValidateCustomResource(field.NewPath("request"), obj, validator)
			// Evaluate transition CEL even when the enum also rejects a changed kind/version.
			celErrs, _ := cel.Validate(context.Background(), field.NewPath("request"), ss, obj, old, celconfig.RuntimeCELCostBudget)
			if tc.celMessage == "" {
				if len(schemaErrs) != 0 || len(celErrs) != 0 {
					t.Fatalf("valid binding rejected: schema=%v CEL=%v", schemaErrs, celErrs)
				}
			} else if len(celErrs) == 0 || !strings.Contains(celErrs.ToAggregate().Error(), tc.celMessage) {
				t.Fatalf("expected transition rejection %q, got schema=%v CEL=%v", tc.celMessage, schemaErrs, celErrs)
			}
		})
	}
	t.Run("sample-12-unbound-create", func(t *testing.T) {
		data, err := os.ReadFile("../../config/samples/12-training-policy.yaml")
		if err != nil {
			t.Fatal(err)
		}
		jsonData, err := yaml.YAMLToJSON(data)
		if err != nil {
			t.Fatal(err)
		}
		decoded := &unstructured.Unstructured{}
		if err := decoded.UnmarshalJSON(jsonData); err != nil {
			t.Fatal(err)
		}
		obj := decoded.Object
		spec := obj["spec"].(map[string]interface{})
		ref := spec["workloadRef"].(map[string]interface{})
		if _, present := ref["uid"]; present {
			t.Fatal("initial sample must omit workload UID")
		}
		aws := spec["capacity"].(map[string]interface{})["aws"].(map[string]interface{})
		if spec["sourceCluster"] != aws["karmadaCluster"] {
			t.Fatal("initial sample must target its AWS member")
		}
		schemaErrs := validation.ValidateCustomResource(field.NewPath("request"), obj, validator)
		celErrs, _ := cel.Validate(context.Background(), field.NewPath("request"), ss, obj, nil, celconfig.RuntimeCELCostBudget)
		if len(schemaErrs) != 0 || len(celErrs) != 0 {
			t.Fatalf("sample rejected: schema=%v CEL=%v", schemaErrs, celErrs)
		}
	})
}
