package management

import (
	"context"
	"errors"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestApplicationTriggerChecksStandbyBeforeQuiescing(t *testing.T) {
	ctx := context.Background()
	cp, _ := groupCheckpointFixture()
	report := groupReport(cp)
	report["clusterName"] = "onpre1"
	report["pods"] = report["pods"].([]interface{})[:1]
	policy := checkpointPolicyFixture(time.Now())
	policy.SetName("policy")
	policy.SetNamespace("demo")
	policy.SetUID("policy")
	_ = unstructured.SetNestedField(policy.Object, "workload", "spec", "workloadRef", "uid")
	_ = unstructured.SetNestedField(policy.Object, "onpre1", "spec", "sourceCluster")
	_ = unstructured.SetNestedMap(policy.Object, map[string]interface{}{"name": "source-runtime"}, "spec", "runtimeRef")
	_ = unstructured.SetNestedMap(policy.Object, map[string]interface{}{
		"mode": "Existing", "aws": map[string]interface{}{"karmadaCluster": "aws"},
		"existingNodes": []interface{}{map[string]interface{}{"rank": int64(0), "nodeName": "standby", "nodeUID": "standby-uid"}},
	}, "spec", "capacity")
	rt := p.NewObject("TrainingRuntime")
	rt.SetName("source-runtime")
	rt.SetNamespace("demo")
	rt.SetUID("rt-uid")
	rt.SetGeneration(1)
	rt.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload"}}
	rt.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{
		"clusterName": "onpre1", "status": map[string]interface{}{
			"observedGeneration": int64(1), "sourceWorldUID": "workload",
			"sourcePods": []interface{}{map[string]interface{}{"name": "trainer-0", "uid": "current-pod", "nodeName": "source-node", "rank": int64(0)}},
		},
	}}}
	sts := p.NewObject("StatefulSet")
	sts.SetName("trainer")
	sts.SetNamespace("demo")
	sts.SetUID("workload")
	sts.Object["spec"] = map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
		"containers": []interface{}{map[string]interface{}{"name": "trainer", "volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}}}},
		"volumes":    []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}}},
	}}}
	binding := placementBinding("onpre1")
	binding.SetName("binding")
	binding.SetNamespace("demo")
	binding.SetUID("binding-uid")
	_ = unstructured.SetNestedField(binding.Object, "workload", "spec", "resource", "uid")
	binding.SetAnnotations(map[string]string{applicationFailoverTrigger: "binding-uid"})
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cp, policy, rt, sts, binding).Build()
	called := false
	r := &GroupPlacementReconciler{Client: c, Reader: c, ExistingNodes: func(context.Context, p.PolicyInput) (map[string]string, error) {
		called = true
		return nil, errors.New("standby not ready")
	}}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(binding)})
	if !called || err == nil || err.Error() != "standby not ready" {
		t.Fatalf("called=%v err=%v", called, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	if policy.GetAnnotations()[groupIntentAnnotation] != "" {
		t.Fatal("quiesced before standby validated")
	}
	for _, kind := range []string{"NodeProvision", "RestoreRequest"} {
		var list *unstructured.UnstructuredList
		if kind == "NodeProvision" {
			list = p.NewList(kind)
		} else {
			list = &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(newRestoreRequest().GroupVersionKind().GroupVersion().WithKind("RestoreRequestList"))
		}
		if err := c.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Fatalf("created %s before standby ready", kind)
		}
	}
	// A stale replay must stop before consulting the standby.
	called = false
	binding.SetAnnotations(map[string]string{applicationFailoverTrigger: "old-binding-uid"})
	if err := c.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(binding)}); err == nil || called {
		t.Fatalf("stale trigger accepted: called=%v err=%v", called, err)
	}
}
