package management

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func groupReleaseFixture(cross bool) (*unstructured.Unstructured, *unstructured.Unstructured) {
	req := newRestoreRequest()
	req.SetName("request")
	req.SetNamespace("demo")
	req.SetUID("request-uid")
	req.SetGeneration(2)
	source := "aws"
	if cross {
		source = "onprem"
	}
	src := []interface{}{map[string]interface{}{"rank": int64(0), "podName": "trainer-0", "podUID": "CURRENT", "nodeName": "old"}}
	req.Object["spec"] = map[string]interface{}{
		"sourceCluster": source, "targetCluster": "aws", "sourceFenced": false, "volumesReady": true,
		"workloadRef":   map[string]interface{}{"name": "trainer", "uid": "world"},
		"checkpointRef": map[string]interface{}{"checkpointID": "round"},
		"pods":          []interface{}{map[string]interface{}{"sourcePod": "trainer-0", "sourcePodUID": "HISTORICAL"}},
		"groupRestore":  map[string]interface{}{"operationUID": "operation", "sourceWorldUID": "world", "worldSize": int64(1), "sourcePods": src},
	}
	control := map[string]interface{}{"operationUID": "operation", "checkpointID": "round", "checkpointGeneration": int64(7), "prepareJobUID": "job", "preparedAt": "2026-09-28T00:01:00Z", "volumeServer": "nfs", "volumePath": "/shared", "volumePVCUID": "pvc", "volumePVUID": "pv"}
	req.Object["status"] = map[string]interface{}{"observedGeneration": int64(2), "phase": "Prepared", "planName": "plan", "groupControl": control}
	plan := &unstructured.Unstructured{}
	plan.SetGroupVersionKind(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "RestorePlan"})
	plan.SetName("plan")
	plan.SetNamespace("demo")
	plan.SetUID("plan-uid")
	plan.SetGeneration(3)
	yes := true
	plan.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: req.GetAPIVersion(), Kind: "RestoreRequest", Name: req.GetName(), UID: req.GetUID(), Controller: &yes}})
	spec, _, _ := unstructured.NestedMap(req.Object, "spec")
	spec["requestUID"] = "request-uid"
	plan.Object["spec"] = spec
	fences := []interface{}{map[string]interface{}{"podName": "trainer-0", "sourcePodUID": "CURRENT", "phase": "SourceGone", "observedGeneration": int64(3), "deleteRequestedAt": "2026-09-28T00:00:00Z", "goneObservedAt": "2026-09-28T00:00:10Z"}}
	report := map[string]interface{}{"observedGeneration": int64(3), "phase": "Prepared", "groupControl": runtime.DeepCopyJSONValue(control), "sourceFences": fences}
	report["clusterName"] = "aws"
	plan.Object["status"] = map[string]interface{}{"clusters": []interface{}{report}}
	if cross {
		receipt := map[string]interface{}{"requestUID": "request-uid", "operationUID": "operation", "sourceWorldUID": "world", "sourceCluster": source, "volumeServer": "nfs", "volumePath": "/shared", "fences": fences}
		b, _ := json.Marshal(receipt)
		plan.SetAnnotations(map[string]string{groupFenceAnnotation: string(b)})
	}
	return req, plan
}

func TestGroupReleaseEvidence(t *testing.T) {
	for _, cross := range []bool{false, true} {
		req, plan := groupReleaseFixture(cross)
		reader := fake.NewClientBuilder().WithObjects(plan).Build()
		if err := validateGroupRelease(context.Background(), reader, req); err != nil {
			t.Fatalf("cross=%v: %v", cross, err)
		}
	}
}

func TestGroupReleaseAcceptsTypedPlanOmittedRankZero(t *testing.T) {
	req, plan := groupReleaseFixture(true)
	req.Object["spec"].(map[string]interface{})["pods"].([]interface{})[0].(map[string]interface{})["rank"] = int64(0)
	reader := fake.NewClientBuilder().WithObjects(plan).Build()
	if err := validateGroupRelease(context.Background(), reader, req); err != nil {
		t.Fatal(err)
	}
}
func TestGroupReleaseRejectsStaleOrUnboundEvidence(t *testing.T) {
	cases := map[string]func(*unstructured.Unstructured, *unstructured.Unstructured){
		"stale request": func(r, p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(r.Object, int64(1), "status", "observedGeneration")
		},
		"request failed": func(r, p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(r.Object, "Failed", "status", "phase")
		},
		"wrong plan name": func(r, p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(r.Object, "other", "status", "planName")
		},
		"wrong owner": func(r, p *unstructured.Unstructured) { p.SetOwnerReferences(nil) },
		"spec drift": func(r, p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, true, "spec", "sourceFenced")
		},
		"duplicate target": func(r, p *unstructured.Unstructured) {
			a, _, _ := unstructured.NestedSlice(p.Object, "status", "clusters")
			_ = unstructured.SetNestedSlice(p.Object, append(a, a[0]), "status", "clusters")
		},
		"stale target": func(r, p *unstructured.Unstructured) { targetReport(p)["observedGeneration"] = int64(2) },
		"missing job": func(r, p *unstructured.Unstructured) {
			targetReport(p)["groupControl"].(map[string]interface{})["prepareJobUID"] = ""
		},
		"backing mismatch": func(r, p *unstructured.Unstructured) {
			targetReport(p)["groupControl"].(map[string]interface{})["volumePath"] = "/another"
		},
		"missing fence receipt": func(r, p *unstructured.Unstructured) { p.SetAnnotations(nil) },
		"historical UID fence": func(r, p *unstructured.Unstructured) {
			var a map[string]interface{}
			_ = json.Unmarshal([]byte(p.GetAnnotations()[groupFenceAnnotation]), &a)
			a["fences"].([]interface{})[0].(map[string]interface{})["sourcePodUID"] = "HISTORICAL"
			b, _ := json.Marshal(a)
			p.SetAnnotations(map[string]string{groupFenceAnnotation: string(b)})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req, plan := groupReleaseFixture(true)
			mutate(req, plan)
			reader := fake.NewClientBuilder().WithObjects(plan).Build()
			if err := validateGroupRelease(context.Background(), reader, req); err == nil {
				t.Fatal("unsafe release accepted")
			}
		})
	}
}
func targetReport(p *unstructured.Unstructured) map[string]interface{} {
	return p.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})
}
