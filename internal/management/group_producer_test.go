package management

import (
	"context"
	"fmt"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGroupReplacementSeparatesCurrentAndArchiveUIDs(t *testing.T) {
	cp, input := groupCheckpointFixture()
	input.PolicyName = "policy"
	input.RuntimeRefName = "runtime"
	input.Capacity.AWSCluster = "aws"
	policy := p.NewObject("TrainingPolicy")
	policy.SetName("policy")
	policy.SetNamespace("demo")
	policy.SetUID(input.PolicyUID)
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: "np-origin-0"})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "np-origin-0", "reason": "group_intent_quiesced"}}
	rt := p.NewObject("TrainingRuntime")
	rt.SetName("runtime")
	rt.SetNamespace("demo")
	rt.SetUID("runtime-uid")
	rt.SetGeneration(1)
	rt.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload"}}
	snapshots := []interface{}{}
	objects := []*unstructured.Unstructured{cp, policy, rt}
	var old *unstructured.Unstructured
	for rank := 0; rank < 2; rank++ {
		name := fmt.Sprintf("trainer-%d", rank)
		node := name + "-node"
		snapshots = append(snapshots, map[string]interface{}{"name": name, "uid": name + "-CURRENT", "nodeName": node, "rank": int64(rank)})
		np := p.NewObject("NodeProvision")
		np.SetNamespace("demo")
		np.SetName(fmt.Sprintf("np-%d", rank))
		np.SetUID(types.UID("np-origin-" + fmt.Sprint(rank)))
		np.SetLabels(map[string]string{p.LabelPolicyUID: "policy"})
		np.Object["spec"] = map[string]interface{}{"marketType": "Spot"}
		np.Object["status"] = map[string]interface{}{"nodeName": node, "instanceId": "instance-" + fmt.Sprint(rank), "memberUID": "np-member-" + fmt.Sprint(rank), "phase": "Ready"}
		objects = append(objects, np)
		if rank == 0 {
			old = np
		}
	}
	rt.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "status": map[string]interface{}{"observedGeneration": int64(1), "sourceWorldUID": "workload", "sourcePods": snapshots, "phase": "Unavailable"}}}}
	sts := p.NewObject("StatefulSet")
	sts.SetNamespace("demo")
	sts.SetName("trainer")
	sts.SetUID("workload")
	sts.Object["spec"] = map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
		"containers": []interface{}{map[string]interface{}{"name": "trainer", "volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}}}},
		"volumes":    []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}}},
	}}}
	replacement := p.NewObject("NodeProvision")
	replacement.SetNamespace("demo")
	replacement.SetName("replacement")
	replacement.SetUID("replacement-uid")
	replacement.SetLabels(map[string]string{p.LabelPolicyUID: "policy"})
	replacement.SetAnnotations(map[string]string{groupOldUID: string(old.GetUID())})
	replacement.Object["spec"] = map[string]interface{}{"marketType": "OnDemand"}
	replacement.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": "new-node"}
	objects = append(objects, sts, replacement)
	fixture := checkpointReconcilerFixture(t, time.Now, objects...)
	r := &PolicyReconciler{Client: fixture.Client, APIReader: fixture.Client}
	created, err := r.ensureGroupReplacement(context.Background(), policy, input, old, "operation", "replacement", "OnDemand", "")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	req := newRestoreRequest()
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "operation-group-restore"}, req); err != nil {
		t.Fatal(err)
	}
	sources, _, _ := unstructured.NestedSlice(req.Object, "spec", "groupRestore", "sourcePods")
	first := sources[0].(map[string]interface{})
	if stringField(first, "podUID") != "trainer-0-CURRENT" || stringField(first, "nodeProvisionRef", "uid") != "np-member-0" {
		t.Fatalf("wrong current source provenance: %#v", first)
	}
	pods, _, _ := unstructured.NestedSlice(req.Object, "spec", "pods")
	archive := pods[0].(map[string]interface{})
	if stringField(archive, "sourcePodUID") != "trainer-0-uid" || stringField(archive, "targetNode") != "new-node" {
		t.Fatalf("archive UID rewritten or target lost: %#v", archive)
	}
	if stringField(pods[1].(map[string]interface{}), "targetNode") != "trainer-1-node" {
		t.Fatal("unaffected rank mapping missing")
	}
	// A repeat must not create another request or advance identity.
	created, err = r.ensureGroupReplacement(context.Background(), policy, input, old, "operation", "replacement", "OnDemand", "")
	if err != nil || created {
		t.Fatalf("retry created=%v err=%v", created, err)
	}
}

func TestGroupReleaseRejectsNonPreparedTarget(t *testing.T) {
	req, plan := groupReleaseFixture(false)
	targetReport(plan)["phase"] = "Running"
	if err := validateGroupPlanRelease(req, plan); err == nil {
		t.Fatal("release accepted a target outside Prepared")
	}
}
