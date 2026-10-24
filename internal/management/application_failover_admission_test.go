package management

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const applicationFailoverTestUser = "system:serviceaccount:hybridspot-system:policy-manager"

func applicationFailoverFixture() (*unstructured.Unstructured, *unstructured.Unstructured, []*unstructured.Unstructured) {
	request, plan := groupReleaseFixture(true)
	policy := p.NewObject("TrainingPolicy")
	policy.SetName("policy")
	policy.SetNamespace("demo")
	policy.SetUID("policy-uid")
	policy.Object["spec"] = map[string]interface{}{
		"workloadRef":   map[string]interface{}{"name": "trainer", "uid": "world"},
		"sourceCluster": "onprem", "targetWorkers": int64(1),
		"capacity": map[string]interface{}{"mode": "Existing", "aws": map[string]interface{}{"karmadaCluster": "aws"}},
	}
	request.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid"})
	old := placementBinding("onprem")
	old.SetName("binding")
	old.SetNamespace("demo")
	old.SetUID("operation")
	_ = unstructured.SetNestedField(old.Object, int64(1), "spec", "replicas")
	_ = unstructured.SetNestedSlice(old.Object, []interface{}{map[string]interface{}{"name": "onprem", "replicas": int64(1)}}, "spec", "clusters")
	old.SetAnnotations(map[string]string{
		applicationFailoverTrigger: "operation", placementRequestAnnotation: request.GetName(), placementRequestUIDAnnotation: string(request.GetUID()),
		groupVolumeHold: string(request.GetUID()), "migration.dcnlab.com/pv-migration": "pv", "migration.dcnlab.com/pv-migration-uid": "pv-uid",
	})
	_ = unstructured.SetNestedField(old.Object, true, "spec", "suspension", "dispatching")
	sts := p.NewObject("StatefulSet")
	sts.SetName("trainer")
	sts.SetNamespace("demo")
	sts.SetUID("world")
	sts.Object["spec"] = map[string]interface{}{"replicas": int64(1), "volumeClaimTemplates": []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data"}}}}
	pv := &unstructured.Unstructured{}
	pv.SetGroupVersionKind(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMigration"})
	pv.SetName("pv")
	pv.SetNamespace("demo")
	pv.SetUID("pv-uid")
	pv.SetGeneration(1)
	pv.Object["spec"] = map[string]interface{}{"sourceCluster": "onprem", "targetCluster": "aws", "resourceBinding": "binding", "sourceFenced": true, "metadataRef": "md", "volumes": []interface{}{map[string]interface{}{"sourcePVC": "data-trainer-0", "targetPVC": "data-trainer-0"}}}
	pv.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "phase": "Completed", "planHash": "hash", "works": []interface{}{map[string]interface{}{"name": "work", "namespace": "karmada-es-aws", "applied": true, "detached": true}}}
	md := &unstructured.Unstructured{}
	md.SetGroupVersionKind(pv.GroupVersionKind().GroupVersion().WithKind("PVMetadata"))
	md.SetName("md")
	md.SetNamespace("demo")
	md.Object["spec"] = map[string]interface{}{"sourceCluster": "onprem", "workloadRef": map[string]interface{}{"name": "trainer", "uid": "world"}}
	return old, policy, []*unstructured.Unstructured{policy, request, plan, sts, pv, md}
}

func applicationFailoverHandler(objects []*unstructured.Unstructured) *PlacementAdmission {
	var items []client.Object
	for _, obj := range objects {
		items = append(items, obj)
	}
	return &PlacementAdmission{Reader: fake.NewClientBuilder().WithObjects(items...).Build(), ReleaseUsername: applicationFailoverTestUser}
}

func applicationFailoverRemoval(old *unstructured.Unstructured) *unstructured.Unstructured {
	next := old.DeepCopy()
	unstructured.RemoveNestedField(next.Object, "spec", "clusters")
	_ = unstructured.SetNestedSlice(next.Object, []interface{}{map[string]interface{}{
		"fromCluster": "onprem", "purgeMode": "Never", "reason": "ApplicationFailure", "suppressDeletion": true,
		"producer": "resource-binding-application-failover-controller", "replicas": int64(1),
	}}, "spec", "gracefulEvictionTasks")
	return next
}

func applicationFailoverAdmit(t *testing.T, h *PlacementAdmission, old, next *unstructured.Unstructured, user, denial string) *unstructured.Unstructured {
	t.Helper()
	oldJSON, err := json.Marshal(old.Object)
	if err != nil {
		t.Fatal(err)
	}
	nextJSON, err := json.Marshal(next.Object)
	if err != nil {
		t.Fatal(err)
	}
	r := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Namespace: old.GetNamespace(), Name: old.GetName(), Operation: admissionv1.Update,
		UserInfo: authenticationv1.UserInfo{Username: user}, OldObject: runtime.RawExtension{Raw: oldJSON}, Object: runtime.RawExtension{Raw: nextJSON},
	}})
	if denial != "" {
		if r.Allowed || r.Result == nil || !strings.Contains(r.Result.Message, denial) {
			t.Fatalf("want denial %q, got %+v", denial, r)
		}
		return nil
	}
	if !r.Allowed {
		t.Fatalf("admission denied: %+v", r.Result)
	}
	if len(r.Patches) != 0 {
		raw, err := json.Marshal(r.Patches)
		if err != nil {
			t.Fatal(err)
		}
		patch, err := jsonpatch.DecodePatch(raw)
		if err != nil {
			t.Fatal(err)
		}
		nextJSON, err = patch.Apply(nextJSON)
		if err != nil {
			t.Fatal(err)
		}
	}
	result := bindingObject()
	if err := utiljson.Unmarshal(nextJSON, &result.Object); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestApplicationFailoverSourceRemovalRequiresPrepared(t *testing.T) {
	for _, tc := range []struct {
		name, denial string
		mutate       func(*unstructured.Unstructured, *unstructured.Unstructured, []*unstructured.Unstructured)
	}{
		{"missing request", "bound restore request missing", func(o, n *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			a := o.GetAnnotations()
			delete(a, placementRequestAnnotation)
			o.SetAnnotations(a)
			n.SetAnnotations(a)
		}},
		{"wrong binding", "ownership mismatch", func(_, _ *unstructured.Unstructured, objects []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(objects[1].Object, "other", "spec", "groupRestore", "operationUID")
		}},
		{"wrong policy", "ownership mismatch", func(_, _ *unstructured.Unstructured, objects []*unstructured.Unstructured) {
			objects[1].SetLabels(map[string]string{p.LabelPolicyUID: "other"})
		}},
		{"wrong workload", "workload mismatch", func(_, _ *unstructured.Unstructured, objects []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(objects[1].Object, "other", "spec", "workloadRef", "uid")
		}},
		{"not prepared", "not prepared", func(_, _ *unstructured.Unstructured, objects []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(objects[1].Object, "Pending", "status", "phase")
		}},
		{"no fence", "fence receipt missing", func(_, _ *unstructured.Unstructured, objects []*unstructured.Unstructured) {
			objects[2].SetAnnotations(nil)
		}},
		{"PV incomplete", "completed PVMigration", func(_, _ *unstructured.Unstructured, objects []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(objects[4].Object, "Running", "status", "phase")
		}},
		{"wrong trigger", "explicit binding UID trigger", func(o, n *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			a := o.GetAnnotations()
			a[applicationFailoverTrigger] = "wrong"
			o.SetAnnotations(a)
			n.SetAnnotations(a)
		}},
		{"not Existing", "Existing capacity", func(_, _ *unstructured.Unstructured, objects []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(objects[0].Object, "Managed", "spec", "capacity", "mode")
		}},
		{"drop dispatch hold", "dispatch suspension", func(_, n *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			unstructured.RemoveNestedField(n.Object, "spec", "suspension")
		}},
		{"drop volume hold", "volume hold", func(_, n *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			a := n.GetAnnotations()
			delete(a, groupVolumeHold)
			n.SetAnnotations(a)
		}},
		{"unsafe task", "ApplicationFailure Never", func(_, n *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			tasks, _, _ := unstructured.NestedSlice(n.Object, "spec", "gracefulEvictionTasks")
			tasks[0].(map[string]interface{})["suppressDeletion"] = false
			_ = unstructured.SetNestedSlice(n.Object, tasks, "spec", "gracefulEvictionTasks")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, _, objects := applicationFailoverFixture()
			next := applicationFailoverRemoval(old)
			tc.mutate(old, next, objects)
			applicationFailoverAdmit(t, applicationFailoverHandler(objects), old, next, "karmada", tc.denial)
		})
	}
}

func TestApplicationFailoverTwoStageAdmission(t *testing.T) {
	old, _, objects := applicationFailoverFixture()
	h := applicationFailoverHandler(objects)
	empty := applicationFailoverAdmit(t, h, old, applicationFailoverRemoval(old), "karmada", "")
	source, err := applicationFailoverSourcePlacement(empty)
	if err != nil || len(source) != 1 || source[0].(map[string]interface{})["replicas"] != int64(1) {
		t.Fatalf("source snapshot lost integer placement: %v %v", source, err)
	}
	if empty.GetAnnotations()[pendingPlacementAnnotation] != "" {
		t.Fatal("fabricated scheduler target")
	}
	for _, mutate := range []func(*unstructured.Unstructured){
		func(n *unstructured.Unstructured) {
			unstructured.RemoveNestedField(n.Object, "spec", "gracefulEvictionTasks")
		},
		func(n *unstructured.Unstructured) {
			tasks, _, _ := unstructured.NestedSlice(n.Object, "spec", "gracefulEvictionTasks")
			tasks[0].(map[string]interface{})["suppressDeletion"] = false
			_ = unstructured.SetNestedSlice(n.Object, tasks, "spec", "gracefulEvictionTasks")
		},
	} {
		next := empty.DeepCopy()
		mutate(next)
		applicationFailoverAdmit(t, h, empty, next, "karmada", "eviction task is immutable")
	}
	metadata := empty.DeepCopy()
	a := metadata.GetAnnotations()
	delete(a, applicationFailoverSourceAnnotation)
	a["ordinary"] = "value"
	metadata.SetAnnotations(a)
	metadata = applicationFailoverAdmit(t, h, empty, metadata, "ordinary", "")
	if metadata.GetAnnotations()[applicationFailoverSourceAnnotation] != empty.GetAnnotations()[applicationFailoverSourceAnnotation] {
		t.Fatal("metadata update removed snapshot")
	}
	for _, mutate := range []func(*unstructured.Unstructured){
		func(n *unstructured.Unstructured) { unstructured.RemoveNestedField(n.Object, "spec", "suspension") },
		func(n *unstructured.Unstructured) {
			a := n.GetAnnotations()
			delete(a, groupVolumeHold)
			n.SetAnnotations(a)
		},
	} {
		next := empty.DeepCopy()
		mutate(next)
		applicationFailoverAdmit(t, h, empty, next, "ordinary", "before release")
	}
	proposed := empty.DeepCopy()
	desired := []interface{}{map[string]interface{}{"name": "aws", "replicas": int64(1)}}
	_ = unstructured.SetNestedSlice(proposed.Object, desired, "spec", "clusters")
	held := applicationFailoverAdmit(t, h, empty, proposed, "scheduler", "")
	clusters, present, err := unstructured.NestedSlice(held.Object, "spec", "clusters")
	if err != nil || !present || clusters == nil || len(clusters) != 0 || held.GetAnnotations()[pendingPlacementAnnotation] == "" {
		t.Fatal("scheduler target not held empty")
	}
	oldTasks, _, _ := unstructured.NestedSlice(empty.Object, "spec", "gracefulEvictionTasks")
	heldTasks, _, _ := unstructured.NestedSlice(held.Object, "spec", "gracefulEvictionTasks")
	if !reflect.DeepEqual(oldTasks, heldTasks) {
		t.Fatal("approved task changed")
	}
	release := held.DeepCopy()
	a = release.GetAnnotations()
	delete(a, pendingPlacementAnnotation)
	delete(a, applicationFailoverSourceAnnotation)
	delete(a, applicationFailoverTrigger)
	delete(a, groupVolumeHold)
	release.SetAnnotations(a)
	unstructured.RemoveNestedField(release.Object, "spec", "suspension", "dispatching")
	_ = unstructured.SetNestedSlice(release.Object, desired, "spec", "clusters")
	applicationFailoverAdmit(t, h, held, release, "ordinary", "only the placement controller")
	// Re-read current preparation evidence at release, even after admission of the empty stage.
	stale := objects[2].DeepCopy()
	stale.SetGeneration(stale.GetGeneration() + 1)
	staleObjects := append([]*unstructured.Unstructured(nil), objects...)
	staleObjects[2] = stale
	applicationFailoverAdmit(t, applicationFailoverHandler(staleObjects), held, release, applicationFailoverTestUser, "stale")
	final := applicationFailoverAdmit(t, h, held, release, applicationFailoverTestUser, "")
	clusters, _, _ = unstructured.NestedSlice(final.Object, "spec", "clusters")
	if !reflect.DeepEqual(clusters, desired) || final.GetAnnotations()[applicationFailoverSourceAnnotation] != "" {
		t.Fatal("verified release did not consume bridge")
	}
}

func TestApplicationFailoverEmptyPlacementRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*unstructured.Unstructured)
	}{
		{"omitted", func(b *unstructured.Unstructured) { unstructured.RemoveNestedField(b.Object, "spec", "clusters") }},
		{"null", func(b *unstructured.Unstructured) { _ = unstructured.SetNestedField(b.Object, nil, "spec", "clusters") }},
		{"array", func(b *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(b.Object, []interface{}{}, "spec", "clusters")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, _, objects := applicationFailoverFixture()
			h := applicationFailoverHandler(objects)
			removal := applicationFailoverRemoval(old)
			tc.set(removal)
			empty := applicationFailoverAdmit(t, h, old, removal, "karmada", "")
			if source, err := applicationFailoverSourcePlacement(empty); err != nil || len(source) != 1 {
				t.Fatalf("source snapshot unavailable: %v, %v", source, err)
			}
			proposed := empty.DeepCopy()
			desired := []interface{}{map[string]interface{}{"name": "aws", "replicas": int64(1)}}
			_ = unstructured.SetNestedSlice(proposed.Object, desired, "spec", "clusters")
			held := applicationFailoverAdmit(t, h, empty, proposed, "scheduler", "")
			// Include legacy null/omitted old objects at the release boundary.
			tc.set(held)
			release := held.DeepCopy()
			a := release.GetAnnotations()
			delete(a, pendingPlacementAnnotation)
			delete(a, applicationFailoverSourceAnnotation)
			delete(a, applicationFailoverTrigger)
			delete(a, groupVolumeHold)
			release.SetAnnotations(a)
			unstructured.RemoveNestedField(release.Object, "spec", "suspension", "dispatching")
			_ = unstructured.SetNestedSlice(release.Object, desired, "spec", "clusters")
			applicationFailoverAdmit(t, h, held, release, "ordinary", "only the placement controller")
			applicationFailoverAdmit(t, h, held, release, applicationFailoverTestUser, "")
			malformed := empty.DeepCopy()
			_ = unstructured.SetNestedField(malformed.Object, "not-a-list", "spec", "clusters")
			applicationFailoverAdmit(t, h, empty, malformed, "scheduler", "expected []interface{}")
		})
	}
}
