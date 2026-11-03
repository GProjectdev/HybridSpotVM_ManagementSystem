package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestGroupVolumeProducer(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		name := "template"
		if standalone {
			name = "shared-and-injected"
		}
		t.Run(name, func(t *testing.T) { testGroupVolumeProducer(t, standalone) })
	}
}

func testGroupVolumeProducer(t *testing.T, standalone bool) {
	ctx := context.Background()
	request, _ := groupReleaseFixture(true)
	binding := placementBinding("onprem")
	binding.SetName("binding")
	binding.SetNamespace("demo")
	binding.SetUID("binding-uid")
	sts := p.NewObject("StatefulSet")
	sts.SetName("trainer")
	sts.SetNamespace("demo")
	sts.SetUID("world")
	sts.Object["spec"] = map[string]interface{}{"volumeClaimTemplates": []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data"}}}}
	expected := []string{"data-trainer-0"}
	if standalone {
		sts.Object["spec"] = map[string]interface{}{"template": map[string]interface{}{
			"metadata": map[string]interface{}{"annotations": map[string]interface{}{"fluidcr.dcnlab.com/checkpoint-claim": "shared"}},
			"spec": map[string]interface{}{"volumes": []interface{}{
				map[string]interface{}{"name": "data", "persistentVolumeClaim": map[string]interface{}{"claimName": "dataset"}},
			}},
		}}
		expected = []string{"dataset", "shared"}
	}
	gvk := schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMetadata"}
	md := &unstructured.Unstructured{}
	md.SetGroupVersionKind(gvk)
	md.SetName("metadata")
	md.SetNamespace("demo")
	md.Object["spec"] = map[string]interface{}{"sourceCluster": "onprem", "workloadRef": map[string]interface{}{"name": "trainer", "uid": "world"}}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("PVMetadataList"), &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts, binding, md).Build()
	r := &GroupPlacementReconciler{Client: c, Reader: c}
	ready, err := r.ensureGroupVolumes(ctx, binding, request)
	if ready || err != nil {
		t.Fatalf("first hold ready=%v err=%v", ready, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
		t.Fatal(err)
	}
	if !boolField(binding.Object, "spec", "suspension", "dispatching") {
		t.Fatal("missing dispatch hold")
	}
	ready, err = r.ensureGroupVolumes(ctx, binding, request)
	if ready || err != nil {
		t.Fatalf("create ready=%v err=%v", ready, err)
	}
	migration := &unstructured.Unstructured{}
	migration.SetGroupVersionKind(gvk.GroupVersion().WithKind("PVMigration"))
	if err := c.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "group-pv-request-uid"}, migration); err != nil {
		t.Fatal(err)
	}
	volumes, _, _ := unstructured.NestedSlice(migration.Object, "spec", "volumes")
	if len(volumes) != len(expected) || !boolField(migration.Object, "spec", "sourceFenced") {
		t.Fatalf("wrong spec %#v", migration.Object["spec"])
	}
	for i, claim := range expected {
		mapping := volumes[i].(map[string]interface{})
		if stringField(mapping, "sourcePVC") != claim || stringField(mapping, "targetPVC") != claim {
			t.Fatalf("wrong shared mapping: %#v", mapping)
		}
	}
	// The API server assigns a UID; fake client does not.
	migration.SetUID("pv-uid")
	if err := c.Update(ctx, migration); err != nil {
		t.Fatal(err)
	}
	ready, err = r.ensureGroupVolumes(ctx, binding, request)
	if ready || err != nil {
		t.Fatalf("bind ready=%v err=%v", ready, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
		t.Fatal(err)
	}
	if binding.GetAnnotations()["migration.dcnlab.com/pv-migration-uid"] != "pv-uid" {
		t.Fatal("missing immutable receipt")
	}
	ready, err = r.ensureGroupVolumes(ctx, binding, request)
	if !ready || err != nil {
		t.Fatalf("retry ready=%v err=%v", ready, err)
	}
	if err := validateGroupVolumes(ctx, c, binding, request); err == nil {
		t.Fatal("new operation released before controller completion")
	}
}
