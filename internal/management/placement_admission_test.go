package management

import (
	"encoding/json"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"reflect"
	"testing"
)

func placementBinding(cluster string) *unstructured.Unstructured {
	o := bindingObject()
	o.Object["spec"] = map[string]interface{}{
		"resource": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "world"},
		"clusters": []interface{}{map[string]interface{}{"name": cluster, "replicas": int64(2)}},
	}
	return o
}

func TestPlacementHeldInSameTransaction(t *testing.T) {
	old, next := placementBinding("onprem"), placementBinding("aws")
	if err := holdPlacement(old, next); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old.Object["spec"], next.Object["spec"]) {
		t.Fatal("placement dispatched before recovery")
	}
	var pending []map[string]interface{}
	if err := json.Unmarshal([]byte(next.GetAnnotations()[pendingPlacementAnnotation]), &pending); err != nil {
		t.Fatal(err)
	}
	if pending[0]["name"] != "aws" {
		t.Fatal(pending)
	}
	// Scheduler retries must retain the same operation intent.
	retry := placementBinding("aws")
	if err := holdPlacement(next, retry); err != nil {
		t.Fatal(err)
	}
	if retry.GetAnnotations()[pendingPlacementAnnotation] != next.GetAnnotations()[pendingPlacementAnnotation] {
		t.Fatal("intent changed")
	}
}
func TestPlacementCannotReplaceInflightTarget(t *testing.T) {
	old := placementBinding("onprem")
	next := placementBinding("aws")
	if err := holdPlacement(old, next); err != nil {
		t.Fatal(err)
	}
	if err := holdPlacement(next, placementBinding("another")); err == nil {
		t.Fatal("overwrote inflight intent")
	}
	removed := next.DeepCopy()
	removed.SetAnnotations(nil)
	if err := holdPlacement(next, removed); err == nil {
		t.Fatal("removed recovery intent")
	}
}
func TestPlacementRejectsSplitOrRecreatedWorld(t *testing.T) {
	for _, mutate := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(o.Object, []interface{}{}, "spec", "clusters")
		},
		func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "other", "spec", "resource", "uid")
		},
	} {
		next := placementBinding("aws")
		mutate(next)
		if err := holdPlacement(placementBinding("onprem"), next); err == nil {
			t.Fatal("accepted unsafe transition")
		}
	}
}
