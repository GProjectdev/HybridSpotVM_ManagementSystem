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
