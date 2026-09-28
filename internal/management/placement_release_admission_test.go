package management

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestPlacementReleaseAdmissionJSONRoundTrip(t *testing.T) {
	const controller = "system:serviceaccount:hybridspot-system:policy-manager"
	for _, tc := range []struct {
		name     string
		username string
		vct      bool
		mutate   func(*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured)
		deny     string
	}{
		{name: "authorized replicas", username: controller},
		{name: "VCT missing volume receipt", username: controller, vct: true, deny: "VCT group restore requires UID-bound PVMigration"},
		{name: "unauthorized user", username: "ordinary-user", deny: "only the placement controller"},
		{name: "stale plan generation", username: controller, mutate: func(_, plan, _ *unstructured.Unstructured) {
			plan.SetGeneration(plan.GetGeneration() + 1)
		}, deny: "target report is missing or stale"},
		{name: "bound request UID mismatch", username: controller, mutate: func(request, _, _ *unstructured.Unstructured) {
			request.SetUID("recreated-request")
		}, deny: "restore request ownership mismatch"},
		{name: "plan request UID mismatch", username: controller, mutate: func(_, plan, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(plan.Object, "foreign-request", "spec", "requestUID")
		}, deny: "restore plan spec differs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, plan := groupReleaseFixture(true)
			old, next := placementBinding("onprem"), placementBinding("aws")
			for _, binding := range []*unstructured.Unstructured{old, next} {
				binding.SetName("trainer-statefulset")
				binding.SetNamespace("demo")
				binding.SetUID("binding-uid")
				// Match the one-rank release fixture, keeping replicas in the wire JSON.
				binding.Object["spec"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})["replicas"] = int64(1)
			}
			for _, object := range []*unstructured.Unstructured{request, plan} {
				if err := unstructured.SetNestedField(object.Object, string(old.GetUID()), "spec", "groupRestore", "operationUID"); err != nil {
					t.Fatal(err)
				}
			}
			if err := unstructured.SetNestedField(request.Object, string(old.GetUID()), "status", "groupControl", "operationUID"); err != nil {
				t.Fatal(err)
			}
			targetReport(plan)["groupControl"].(map[string]interface{})["operationUID"] = string(old.GetUID())
			var receipt map[string]interface{}
			if err := json.Unmarshal([]byte(plan.GetAnnotations()[groupFenceAnnotation]), &receipt); err != nil {
				t.Fatal(err)
			}
			receipt["operationUID"] = string(old.GetUID())
			encodedReceipt, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			plan.SetAnnotations(map[string]string{groupFenceAnnotation: string(encodedReceipt)})

			policy := p.NewObject("TrainingPolicy")
			policy.SetName("policy")
			policy.SetNamespace("demo")
			policy.SetUID("policy-uid")
			policy.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"name": "trainer", "uid": "world"}}
			request.SetLabels(map[string]string{p.LabelPolicyUID: string(policy.GetUID())})
			clusters, _, err := unstructured.NestedSlice(next.Object, "spec", "clusters")
			if err != nil {
				t.Fatal(err)
			}
			pending, err := json.Marshal(clusters)
			if err != nil {
				t.Fatal(err)
			}
			old.SetAnnotations(map[string]string{
				pendingPlacementAnnotation:    string(pending),
				placementRequestAnnotation:    request.GetName(),
				placementRequestUIDAnnotation: string(request.GetUID()),
			})
			next.SetAnnotations(map[string]string{
				placementRequestAnnotation:    request.GetName(),
				placementRequestUIDAnnotation: string(request.GetUID()),
			})
			if tc.mutate != nil {
				tc.mutate(request, plan, next)
			}
			oldJSON, err := json.Marshal(old.Object)
			if err != nil {
				t.Fatal(err)
			}
			nextJSON, err := json.Marshal(next.Object)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(nextJSON), `"replicas":1`) {
				t.Fatal("replicas missing from admission JSON")
			}
			sts := p.NewObject("StatefulSet")
			sts.SetName("trainer")
			sts.SetNamespace("demo")
			sts.SetUID("world")
			if tc.vct {
				sts.Object["spec"] = map[string]interface{}{"volumeClaimTemplates": []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data"}}}}
			}
			handler := &PlacementAdmission{Reader: fake.NewClientBuilder().WithObjects(policy, request, plan, sts).Build(), ReleaseUsername: controller}
			response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Namespace: "demo", Name: old.GetName(), Operation: admissionv1.Update,
				UserInfo:  authenticationv1.UserInfo{Username: tc.username},
				OldObject: runtime.RawExtension{Raw: oldJSON}, Object: runtime.RawExtension{Raw: nextJSON},
			}})
			if tc.deny == "" {
				if !response.Allowed {
					t.Fatalf("valid JSON release denied: %+v", response.Result)
				}
				if response.Result == nil || !strings.Contains(response.Result.Message, "UID-bound group restore Prepared") {
					t.Fatalf("release bypassed evidence validation: %+v", response.Result)
				}
				if len(response.Patches) != 0 {
					t.Fatalf("authorized release unexpectedly mutated: %+v", response.Patches)
				}
			} else {
				if response.Allowed {
					t.Fatal("unsafe release admitted")
				}
				if response.Result == nil || !strings.Contains(response.Result.Message, tc.deny) {
					t.Fatalf("unexpected denial, want %q: %+v", tc.deny, response.Result)
				}
			}
		})
	}
}
