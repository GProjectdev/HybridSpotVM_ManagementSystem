package management

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGroupPlacementRequestsAllColdCapacityBeforeWaiting(t *testing.T) {
	ctx := context.Background()
	cp, _ := groupCheckpointFixture()
	groupReport(cp)["clusterName"] = "onpre1"
	policy := checkpointPolicyFixture(time.Now())
	policy.SetName("policy")
	policy.SetNamespace("demo")
	policy.SetUID("policy")
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: "binding-uid"})
	set := func(obj *unstructured.Unstructured, value interface{}, fields ...string) {
		t.Helper()
		if err := unstructured.SetNestedField(obj.Object, value, fields...); err != nil {
			t.Fatal(err)
		}
	}
	set(policy, "workload", "spec", "workloadRef", "uid")
	set(policy, "workload", "status", "discovery", "workloadUID")
	set(policy, "onpre1", "spec", "sourceCluster")
	set(policy, int64(2), "spec", "targetWorkers")
	set(policy, map[string]interface{}{"name": "source-runtime"}, "spec", "runtimeRef")
	set(policy, map[string]interface{}{
		"mode": "Provisioned", "aws": map[string]interface{}{
			"karmadaCluster": "aws", "hardwareType": "gpu", "nodeLabel": "gpu", "instanceType": "g4dn.xlarge",
		},
	}, "spec", "capacity")
	// Start after the checkpoint coordinator has acknowledged quiescence.
	set(policy, map[string]interface{}{
		"periodicQuiesced": true, "replacementOperation": "binding-uid", "reason": "group_intent_quiesced",
	}, "status", "checkpoint")
	rt := p.NewObject("TrainingRuntime")
	rt.SetName("source-runtime")
	rt.SetNamespace("demo")
	rt.SetUID("source-runtime-uid")
	rt.SetGeneration(1)
	rt.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload"}}
	sources := []interface{}{}
	for rank := int64(0); rank < 2; rank++ {
		sources = append(sources, map[string]interface{}{
			"name": fmt.Sprintf("trainer-%d", rank), "uid": fmt.Sprintf("current-pod-%d", rank),
			"nodeName": fmt.Sprintf("source-node-%d", rank), "rank": rank,
		})
	}
	rt.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{
		"clusterName": "onpre1", "status": map[string]interface{}{
			"observedGeneration": int64(1), "sourceWorldUID": "workload", "sourcePods": sources,
		},
	}}}
	sts := p.NewObject("StatefulSet")
	sts.SetName("trainer")
	sts.SetNamespace("demo")
	sts.SetUID("workload")
	sts.Object["spec"] = map[string]interface{}{"replicas": int64(2), "template": map[string]interface{}{"spec": map[string]interface{}{
		"containers": []interface{}{map[string]interface{}{
			"name": "trainer", "env": []interface{}{map[string]interface{}{"name": "FLUIDCR_CHECKPOINT_DIR", "value": "/checkpoint"}},
			"volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}},
		}},
		"volumes": []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}}},
	}}}
	binding := placementBinding("onpre1")
	binding.SetName("binding")
	binding.SetNamespace("demo")
	binding.SetUID("binding-uid")
	set(binding, "workload", "spec", "resource", "uid")
	set(binding, int64(2), "spec", "replicas")
	set(binding, []interface{}{map[string]interface{}{"name": "onpre1", "replicas": int64(2)}}, "spec", "clusters")
	binding.SetAnnotations(map[string]string{pendingPlacementAnnotation: `[{"name":"aws","replicas":2}]`})
	// The fake API does not assign UIDs to newly created objects. Seed the exact
	// target runtime that discovery would create so request construction has one.
	targetRuntime := p.NewObject("TrainingRuntime")
	targetRuntime.SetName(runtimeName(policy.GetName(), "aws"))
	targetRuntime.SetNamespace("demo")
	targetRuntime.SetUID("target-runtime-uid")
	targetRuntime.SetLabels(map[string]string{p.LabelPolicyUID: "policy"})
	targetRuntime.Object["spec"] = map[string]interface{}{
		"workloadRef":   map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload"},
		"sourceCluster": "aws", "expectedWorldSize": int64(2), "port": int64(8298), "container": "trainer",
	}
	scheme := testScheme()
	requests := &unstructured.UnstructuredList{}
	requests.SetGroupVersionKind(newRestoreRequest().GroupVersionKind().GroupVersion().WithKind("RestoreRequestList"))
	scheme.AddKnownTypes(newRestoreRequest().GroupVersionKind().GroupVersion(), newRestoreRequest(), requests)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(p.NewObject("NodeProvision")).
		WithObjects(cp, policy, rt, sts, binding, targetRuntime).Build()
	r := &GroupPlacementReconciler{Client: c, Reader: c}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(binding)}); err != nil {
			t.Fatal(err)
		}
	}
	assertRequestCount := func(want int) {
		t.Helper()
		if err := c.List(ctx, requests, client.InNamespace("demo")); err != nil {
			t.Fatal(err)
		}
		if len(requests.Items) != want {
			t.Fatalf("RestoreRequests = %d, want %d", len(requests.Items), want)
		}
	}
	input := p.ReadPolicyInput(policy)
	assertCapacity := func(wantPolicies int) {
		t.Helper()
		for kind, want := range map[string]int{"NodeProvision": 2, "PropagationPolicy": wantPolicies} {
			list := p.NewList(kind)
			if err := c.List(ctx, list, client.InNamespace("demo")); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != want {
				t.Fatalf("%s count = %d, want %d; all ranks must be requested before waiting", kind, len(list.Items), want)
			}
		}
		for rank := int64(0); rank < 2; rank++ {
			np := p.NewNodeProvision(input, rank, "OnDemand")
			pp := p.NewPropagationPolicyFor(input, np, "aws")
			for _, expected := range []*unstructured.Unstructured{np, pp} {
				actual := expected.DeepCopy()
				if err := c.Get(ctx, client.ObjectKeyFromObject(expected), actual); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(actual.Object["spec"], expected.Object["spec"]) || !reflect.DeepEqual(actual.GetLabels(), expected.GetLabels()) {
					t.Fatalf("%s %s differs from expected rank capacity/placement", actual.GetKind(), actual.GetName())
				}
			}
		}
	}
	markReady := func(rank int64, node string) {
		t.Helper()
		np := p.NewNodeProvision(input, rank, "OnDemand")
		if err := c.Get(ctx, client.ObjectKeyFromObject(np), np); err != nil {
			t.Fatal(err)
		}
		np.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": node}
		if err := c.Status().Update(ctx, np); err != nil {
			t.Fatal(err)
		}
	}

	reconcile()
	assertCapacity(2)
	assertRequestCount(0)
	before := p.NewList("NodeProvision")
	if err := c.List(ctx, before, client.InNamespace("demo")); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		reconcile()
		assertCapacity(2)
		assertRequestCount(0)
	}
	for _, original := range before.Items {
		actual := p.NewObject("NodeProvision")
		if err := c.Get(ctx, client.ObjectKeyFromObject(&original), actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(original.Object, actual.Object) {
			t.Fatalf("repeated pending reconcile mutated NodeProvision %s", original.GetName())
		}
	}
	markReady(0, "aws-node-0")
	reconcile()
	assertCapacity(2)
	assertRequestCount(0)
	markReady(1, "")
	reconcile()
	assertRequestCount(0)
	markReady(1, "aws-node-1")
	reconcile()
	assertCapacity(3) // Discovery also creates the target runtime's policy.
	assertRequestCount(1)
	request := &requests.Items[0]
	if request.GetName() != groupPlacementRequestName(policy.GetName(), string(binding.GetUID()), "aws") ||
		stringField(request.Object, "spec", "sourceCluster") != "onpre1" || stringField(request.Object, "spec", "targetCluster") != "aws" ||
		stringField(request.Object, "spec", "groupRestore", "operationUID") != string(binding.GetUID()) ||
		intField(request.Object, "spec", "groupRestore", "worldSize") != 2 {
		t.Fatal("restore request lost the full-world placement identity")
	}
	pods, _, err := unstructured.NestedSlice(request.Object, "spec", "pods")
	if err != nil || len(pods) != 2 {
		t.Fatalf("restore pods = %v, error = %v", pods, err)
	}
	for rank, raw := range pods {
		pod := raw.(map[string]interface{})
		if stringField(pod, "targetNode") != fmt.Sprintf("aws-node-%d", rank) || stringField(pod, "targetPod") != fmt.Sprintf("trainer-%d", rank) {
			t.Fatalf("wrong rank/node mapping: %v", pod)
		}
	}
}
