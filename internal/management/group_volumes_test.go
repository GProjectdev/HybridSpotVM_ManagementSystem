package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestGroupVolumes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured)
		reject bool
	}{
		{name: "shared checkpoint only", mutate: func(s, b, p, m *unstructured.Unstructured) {
			unstructured.RemoveNestedField(s.Object, "spec", "volumeClaimTemplates")
			b.SetAnnotations(nil)
		}},
		{name: "ordinal claims ready"},
		{name: "mixed split claims ready", mutate: func(s, b, p, m *unstructured.Unstructured) { addSplitReceipt(s, p) }},
		{name: "shared-only split claims ready", mutate: func(s, b, p, m *unstructured.Unstructured) {
			addSplitReceipt(s, p)
			unstructured.RemoveNestedField(s.Object, "spec", "volumeClaimTemplates")
			volumes, _, _ := unstructured.NestedSlice(p.Object, "spec", "volumes")
			works, _, _ := unstructured.NestedSlice(p.Object, "status", "works")
			_ = unstructured.SetNestedSlice(p.Object, volumes[1:], "spec", "volumes")
			_ = unstructured.SetNestedSlice(p.Object, works[1:], "status", "works")
		}},
		{name: "split half missing", mutate: func(s, b, p, m *unstructured.Unstructured) {
			addSplitReceipt(s, p)
			works, _, _ := unstructured.NestedSlice(p.Object, "status", "works")
			_ = unstructured.SetNestedSlice(p.Object, works[:2], "status", "works")
		}, reject: true},
		{name: "split UID mismatch", mutate: func(s, b, p, m *unstructured.Unstructured) {
			addSplitReceipt(s, p)
			p.Object["status"].(map[string]interface{})["works"].([]interface{})[2].(map[string]interface{})["pvcUID"] = "other"
		}, reject: true},
		{name: "split receipt missing UID", mutate: func(s, b, p, m *unstructured.Unstructured) {
			addSplitReceipt(s, p)
			p.Object["status"].(map[string]interface{})["works"].([]interface{})[1].(map[string]interface{})["pvUID"] = ""
		}, reject: true},
		{name: "split duplicate", mutate: func(s, b, p, m *unstructured.Unstructured) {
			addSplitReceipt(s, p)
			works := p.Object["status"].(map[string]interface{})["works"].([]interface{})
			works[2] = works[1]
		}, reject: true},
		{name: "split wrong name", mutate: func(s, b, p, m *unstructured.Unstructured) {
			addSplitReceipt(s, p)
			p.Object["status"].(map[string]interface{})["works"].([]interface{})[2].(map[string]interface{})["name"] = "foreign-pvc"
		}, reject: true},
		{name: "missing receipt", mutate: func(s, b, p, m *unstructured.Unstructured) { b.SetAnnotations(nil) }, reject: true},
		{name: "stale PV generation", mutate: func(s, b, p, m *unstructured.Unstructured) { p.SetGeneration(2) }, reject: true},
		{name: "recreated PV operation", mutate: func(s, b, p, m *unstructured.Unstructured) { p.SetUID("other") }, reject: true},
		{name: "wrong workload", mutate: func(s, b, p, m *unstructured.Unstructured) { s.SetUID("other") }, reject: true},
		{name: "wrong metadata world", mutate: func(s, b, p, m *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(m.Object, "other", "spec", "workloadRef", "uid")
		}, reject: true},
		{name: "missing mapping", mutate: func(s, b, p, m *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(p.Object, nil, "spec", "volumes")
		}, reject: true},
		{name: "undetached work", mutate: func(s, b, p, m *unstructured.Unstructured) {
			p.Object["status"].(map[string]interface{})["works"].([]interface{})[0].(map[string]interface{})["detached"] = false
		}, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := groupReleaseFixture(true)
			sts := p.NewObject("StatefulSet")
			sts.SetName("trainer")
			sts.SetNamespace("demo")
			sts.SetUID("world")
			sts.Object["spec"] = map[string]interface{}{"volumeClaimTemplates": []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data"}}}}
			binding := placementBinding("onprem")
			binding.SetName("binding")
			binding.SetAnnotations(map[string]string{"migration.dcnlab.com/pv-migration": "pv", "migration.dcnlab.com/pv-migration-uid": "pv-uid"})
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
			if tc.mutate != nil {
				tc.mutate(sts, binding, pv, md)
			}
			err := validateGroupVolumes(context.Background(), fake.NewClientBuilder().WithObjects(sts, pv, md).Build(), binding, req)
			if (err != nil) != tc.reject {
				t.Fatalf("reject=%v err=%v", tc.reject, err)
			}
		})
	}
}

func addSplitReceipt(sts, migration *unstructured.Unstructured) {
	_ = unstructured.SetNestedField(sts.Object, "shared", "spec", "template", "metadata", "annotations", "fluidcr.dcnlab.com/checkpoint-claim")
	volumes, _, _ := unstructured.NestedSlice(migration.Object, "spec", "volumes")
	volumes = append(volumes, map[string]interface{}{"sourcePVC": "shared", "targetPVC": "shared"})
	_ = unstructured.SetNestedSlice(migration.Object, volumes, "spec", "volumes")
	works, _, _ := unstructured.NestedSlice(migration.Object, "status", "works")
	name := groupPVWorkName(string(sts.GetUID()), "aws", sts.GetNamespace(), "shared")
	for _, n := range []string{name, name + "-pvc"} {
		works = append(works, map[string]interface{}{"name": n, "namespace": "karmada-es-aws", "standalone": true, "applied": true, "detached": true, "pvUID": "member-pv", "pvcUID": "member-pvc"})
	}
	_ = unstructured.SetNestedSlice(migration.Object, works, "status", "works")
}
