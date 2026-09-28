package management

import (
	"context"
	"encoding/json"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestExplicitStopPlacement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured)
		blocker  string
		allow    bool
		terminal bool
		stale    bool
	}{
		{name: "empty scheduler placement", allow: true},
		{name: "omitted binding zero", allow: true, mutate: func(_, next, _ *unstructured.Unstructured) {
			unstructured.RemoveNestedField(next.Object, "spec", "replicas")
		}},
		{name: "terminal checkpoint", blocker: "FluidCRMigration", terminal: true, allow: true},
		{name: "stale terminal checkpoint", blocker: "FluidCRMigration", terminal: true, stale: true},
		{name: "expanded scheduler placement", allow: true, mutate: func(_, next, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(next.Object, []interface{}{map[string]interface{}{"name": "aws", "replicas": int64(0)}, map[string]interface{}{"name": "onprem", "replicas": int64(0)}}, "spec", "clusters")
		}},
		{name: "nonzero workload", mutate: func(_, _, sts *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(sts.Object, int64(2), "spec", "replicas")
		}},
		{name: "implicit replicas", mutate: func(_, _, sts *unstructured.Unstructured) {
			unstructured.RemoveNestedField(sts.Object, "spec", "replicas")
		}},
		{name: "stale workload UID", mutate: func(_, _, sts *unstructured.Unstructured) { sts.SetUID("recreated") }},
		{name: "nonzero binding", mutate: func(_, next, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(next.Object, int64(2), "spec", "replicas")
		}},
		{name: "pending restore", mutate: func(old, next, _ *unstructured.Unstructured) {
			old.SetAnnotations(map[string]string{pendingPlacementAnnotation: "[]"})
			next.SetAnnotations(old.GetAnnotations())
		}},
		{name: "restore request", blocker: "RestoreRequest"},
		{name: "restore plan", blocker: "RestorePlan"},
		{name: "replacement", blocker: "SpotReplacement"},
		{name: "checkpoint", blocker: "FluidCRMigration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, next := placementBinding("aws"), placementBinding("aws")
			for _, b := range []*unstructured.Unstructured{old, next} {
				b.SetName("trainer-statefulset")
				b.SetNamespace("demo")
			}
			_ = unstructured.SetNestedSlice(next.Object, []interface{}{}, "spec", "clusters")
			_ = unstructured.SetNestedField(next.Object, int64(0), "spec", "replicas")
			sts := p.NewObject("StatefulSet")
			sts.SetName("trainer")
			sts.SetNamespace("demo")
			sts.SetUID("world")
			_ = unstructured.SetNestedField(sts.Object, int64(0), "spec", "replicas")
			policy := p.NewObject("TrainingPolicy")
			policy.SetName("policy")
			policy.SetNamespace("demo")
			policy.SetUID("policy-uid")
			policy.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"name": "trainer", "uid": "world"}}
			if tc.mutate != nil {
				tc.mutate(old, next, sts)
			}
			objects := []client.Object{policy, sts}
			plan := newRestoreRequest()
			plan.SetKind("RestorePlan")
			builder := fake.NewClientBuilder()
			for _, prototype := range []*unstructured.Unstructured{newSpotReplacementObject(), newRestoreRequest(), plan, p.NewObject("FluidCRMigration")} {
				list := &unstructured.UnstructuredList{}
				gvk := prototype.GroupVersionKind()
				gvk.Kind += "List"
				list.SetGroupVersionKind(gvk)
				builder.WithLists(list)
				if prototype.GetKind() == tc.blocker {
					prototype.SetName("blocking")
					prototype.SetNamespace("demo")
					_ = unstructured.SetNestedField(prototype.Object, "world", "spec", "workloadRef", "uid")
					if tc.terminal {
						prototype.SetGeneration(2)
						observed := int64(2)
						if tc.stale {
							observed = 1
						}
						_ = unstructured.SetNestedSlice(prototype.Object, []interface{}{map[string]interface{}{"clusterName": "aws", "phase": "Completed", "observedGeneration": observed}}, "status", "clusters")
					}
					objects = append(objects, prototype)
				}
			}
			h := &PlacementAdmission{Reader: builder.WithObjects(objects...).Build()}
			oldJSON, _ := json.Marshal(old.Object)
			nextJSON, _ := json.Marshal(next.Object)
			response := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Update, Namespace: "demo", OldObject: runtime.RawExtension{Raw: oldJSON}, Object: runtime.RawExtension{Raw: nextJSON}}})
			if response.Allowed != tc.allow {
				t.Fatalf("allowed=%v result=%v", response.Allowed, response.Result)
			}
			if tc.allow {
				handled, err := h.holdStoppedPlacement(context.Background(), policy, old, next)
				if !handled || err != nil {
					t.Fatalf("handled=%v err=%v", handled, err)
				}
				clusters, _, _ := unstructured.NestedSlice(next.Object, "spec", "clusters")
				if len(clusters) != 1 {
					t.Fatal(clusters)
				}
				source := clusters[0].(map[string]interface{})
				if source["name"] != "aws" || source["replicas"] != int64(0) {
					t.Fatal(source)
				}
				before, _, _ := unstructured.NestedSlice(old.Object, "spec", "clusters")
				if before[0].(map[string]interface{})["replicas"] != int64(2) {
					t.Fatal("mutated old binding")
				}
			}
		})
	}
}
