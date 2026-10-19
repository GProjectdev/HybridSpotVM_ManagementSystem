package manifests_test

import (
	"os"
	"reflect"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structural "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func TestEmergencyFallbackStatusSurvivesPruning(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/spotreplacements.yaml")
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
	s, err := structural.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]interface{}{"name": "round", "uid": "round-uid", "generation": int64(1), "checkpointID": "round-id"}
	fallback := map[string]interface{}{
		"reason": "interruption_deadline_incomplete_partial", "interruptionTime": "2026-10-19T00:02:00Z",
		"checkpointRef": ref, "interruptedPartialCheckpointRef": runtime.DeepCopyJSONValue(ref),
		"sourcePods": []interface{}{map[string]interface{}{
			"rank": int64(0), "podName": "trainer-0", "podUID": "pod-uid", "nodeName": "worker",
			"nodeProvisionRef": map[string]interface{}{"name": "worker", "uid": "node-uid", "instanceID": "i-1"},
		}},
	}
	want := runtime.DeepCopyJSONValue(fallback)
	obj := map[string]interface{}{"status": map[string]interface{}{"partialFallback": fallback}}
	pruning.Prune(obj, s, true)
	if !reflect.DeepEqual(want, obj["status"].(map[string]interface{})["partialFallback"]) {
		t.Fatalf("fallback evidence pruned: %#v", obj)
	}
}
