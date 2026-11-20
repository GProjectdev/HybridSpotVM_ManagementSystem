package management

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func batchFixture(t *testing.T, reverse bool) (*PolicyReconciler, *unstructured.Unstructured, p.PolicyInput, groupOperation) {
	t.Helper()
	cp, input := groupCheckpointFixture()
	input.TargetWorkers = 4
	input.PolicyName = "policy"
	input.RuntimeRefName = "runtime"
	input.Capacity.AWSCluster = "aws"
	input.Checkpoint.Resume = true
	policy := p.NewObject("NodeRequest")
	policy.SetNamespace("demo")
	policy.SetName("policy")
	policy.SetUID(input.PolicyUID)
	policy.SetGeneration(1)
	policy.Object["spec"] = deepCopyMap(policyFixtureForCapacity(time.Now(), 4).Object["spec"].(map[string]interface{}))
	_ = unstructured.SetNestedField(policy.Object, true, "spec", "replacement", "enabled")
	_ = unstructured.SetNestedField(policy.Object, "aws", "spec", "sourceCluster")
	_ = unstructured.SetNestedMap(policy.Object, map[string]interface{}{"name": "runtime"}, "spec", "runtimeRef")
	_ = unstructured.SetNestedMap(policy.Object, map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload"}, "spec", "workloadRef")
	policy.SetAnnotations(map[string]string{groupBatchSizeAnnotation: "3", "training.dcnlab.com/planned-partial": "disabled", groupIntentAnnotation: "old-1"})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "old-1", "reason": "group_intent_quiesced"}}
	rt := p.NewObject("TrainingRuntime")
	rt.SetNamespace("demo")
	rt.SetName("runtime")
	rt.SetUID("runtime-uid")
	rt.SetGeneration(1)
	rt.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload"}}
	sts := p.NewObject("StatefulSet")
	sts.SetNamespace("demo")
	sts.SetName("trainer")
	sts.SetUID("workload")
	attachCheckpointVolume(sts)
	snapshots, pods := []interface{}{}, []interface{}{}
	objects := []*unstructured.Unstructured{cp, policy, rt, sts}
	intent := groupOperation{PolicyUID: "policy", PolicyGeneration: 1}
	base := groupReport(cp)["pods"].([]interface{})[0].(map[string]interface{})
	for rank := 0; rank < 4; rank++ {
		name := fmt.Sprintf("policy-worker-%02d", rank)
		uid := fmt.Sprintf("old-%d", rank)
		node := fmt.Sprintf("node-%d", rank)
		old := p.NewObject("NodeProvision")
		old.SetNamespace("demo")
		old.SetName(name)
		old.SetUID(types.UID(uid))
		old.SetLabels(map[string]string{p.LabelPolicyUID: "policy", p.LabelRole: "worker"})
		market := "Spot"
		if reverse || rank == 0 {
			market = "OnDemand"
		}
		old.Object["spec"] = map[string]interface{}{"marketType": market, "awsConfig": map[string]interface{}{"keyPairName": "source-key"}}
		old.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": node, "instanceId": "i-" + uid, "memberUID": "member-" + uid}
		objects = append(objects, old)
		snapshots = append(snapshots, map[string]interface{}{"rank": int64(rank), "name": fmt.Sprintf("trainer-%d", rank), "uid": fmt.Sprintf("current-%d", rank), "nodeName": node})
		pod := deepCopyMap(base)
		pod["podName"] = fmt.Sprintf("trainer-%d", rank)
		pod["podUID"] = fmt.Sprintf("archive-%d", rank)
		pod["nodeName"] = node
		pods = append(pods, pod)
		if rank > 0 {
			desired := "OnDemand"
			if reverse {
				desired = "Spot"
			}
			intent.Batch = append(intent.Batch, groupBatchTarget{name, uid, replacementNodeProvisionName(name, uid), desired})
		}
	}
	first := intent.Batch[0]
	intent.OldName, intent.OldUID, intent.Replacement, intent.Market = first.OldName, first.OldUID, first.Replacement, first.Market
	intent.Operation = replacementOperationName(first.OldName, first.OldUID)
	groupReport(cp)["pods"] = pods
	rt.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "status": map[string]interface{}{"observedGeneration": int64(1), "sourceWorldUID": "workload", "sourcePods": snapshots}}}}
	f := checkpointReconcilerFixture(t, time.Now, objects...)
	return &PolicyReconciler{Client: f.Client, APIReader: f.Client, GroupStage: groupStageCapacity}, policy, input, intent
}

func readyBatchTargets(t *testing.T, r *PolicyReconciler, intent groupOperation, count int) {
	t.Helper()
	for i, target := range intent.Batch[:count] {
		np := p.NewObject("NodeProvision")
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: target.Replacement}, np); err != nil {
			t.Fatal(err)
		}
		np.SetUID(types.UID(fmt.Sprintf("new-%d", i)))
		np.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": fmt.Sprintf("new-node-%d", i)}
		if err := r.Update(context.Background(), np); err != nil {
			t.Fatal(err)
		}
	}
	if count == len(intent.Batch) {
		completePlannedBatchCheckpoint(t, r, intent)
	}
}

func batchRequest(t *testing.T, r *PolicyReconciler, intent groupOperation) *unstructured.Unstructured {
	t.Helper()
	req := newRestoreRequest()
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: intent.Operation + "-group-restore"}, req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestPolicyBatchPreparesThreeVMsAndUsesOneFourRankRestoreBothDirections(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			ctx := context.Background()
			r, policy, input, intent := batchFixture(t, reverse)
			for i := 0; i < 2; i++ {
				if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
					t.Fatal(err)
				}
			}
			list := p.NewList("NodeProvision")
			if err := r.List(ctx, list, client.InNamespace("demo")); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 7 {
				t.Fatalf("provider inventory=%d, want4+3", len(list.Items))
			}
			for _, np := range list.Items {
				if np.GetLabels()[p.LabelRole] == groupReplacementRole && stringField(np.Object, "spec", "awsConfig", "keyPairName") != "" {
					t.Fatal("source key reused")
				}
			}
			readyBatchTargets(t, r, intent, 2)
			r.GroupStage = groupStageRequest
			if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err == nil {
				t.Fatal("incomplete capacity accepted")
			}
			if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: intent.Operation + "-group-restore"}, newRestoreRequest()); !apierrors.IsNotFound(err) {
				t.Fatal("restore before all targets ready")
			}
			readyBatchTargets(t, r, intent, 3)
			created, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent)
			if err != nil || !created {
				t.Fatalf("created=%v err=%v", created, err)
			}
			req := batchRequest(t, r, intent)
			pods, _, _ := unstructured.NestedSlice(req.Object, "spec", "pods")
			sources, _, _ := unstructured.NestedSlice(req.Object, "spec", "groupRestore", "sourcePods")
			if len(pods) != 4 || len(sources) != 4 {
				t.Fatal("full world missing")
			}
			for i, raw := range pods {
				pod := raw.(map[string]interface{})
				want := "node-0"
				if i > 0 {
					want = fmt.Sprintf("new-node-%d", i-1)
				}
				if stringField(pod, "targetNode") != want || stringField(pod, "sourcePodUID") != fmt.Sprintf("current-%d", i) || stringField(sources[i].(map[string]interface{}), "podUID") != fmt.Sprintf("current-%d", i) {
					t.Fatalf("rank %d mapping/provenance changed: %#v", i, pod)
				}
			}
			pairs, err := groupReplacementPairs(req)
			if err != nil || len(pairs) != 3 {
				t.Fatalf("pairs=%v err=%v", pairs, err)
			}
			created, err = r.ensureGroupBatchReplacement(ctx, policy, input, intent)
			if err != nil || created {
				t.Fatal("retry created duplicate restore")
			}
			for _, pair := range pairs {
				if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: pair.OldName}, p.NewObject("NodeProvision")); err != nil {
					t.Fatal("source removed before verification")
				}
			}
		})
	}
}

func TestPolicyBatchRejectsUnsafeInputsBeforeCreatingVMs(t *testing.T) {
	for _, kind := range []string{"duplicate-source", "stale-policy", "wrong-old-uid", "partial-enabled", "emergency", "over-limit", "no-checkpoint", "suspended"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, policy, input, intent := batchFixture(t, false)
			switch kind {
			case "duplicate-source":
				intent.Batch[2] = intent.Batch[1]
			case "stale-policy":
				intent.PolicyGeneration++
			case "wrong-old-uid":
				intent.Batch[2].OldUID = "recreated"
				intent.Batch[2].Replacement = replacementNodeProvisionName(intent.Batch[2].OldName, "recreated")
			case "partial-enabled":
				a := policy.GetAnnotations()
				delete(a, "training.dcnlab.com/planned-partial")
				policy.SetAnnotations(a)
			case "emergency":
				intent.Event = "notice"
			case "over-limit":
				a := policy.GetAnnotations()
				a[groupBatchSizeAnnotation] = "2"
				policy.SetAnnotations(a)
			case "no-checkpoint":
				cp := p.NewObject("FluidCRMigration")
				cp.SetNamespace("demo")
				cp.SetName("round")
				if err := r.Delete(ctx, cp); err != nil {
					t.Fatal(err)
				}
			case "suspended":
				input.Suspended = true
			}
			if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err == nil {
				t.Fatal("unsafe batch accepted")
			}
			list := p.NewList("NodeProvision")
			if err := r.List(ctx, list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 4 {
				t.Fatal("allocated capacity before validating full batch")
			}
		})
	}
}

func markBatchVerified(t *testing.T, r *PolicyReconciler, req *unstructured.Unstructured) {
	t.Helper()
	req.SetUID("restore-uid")
	req.SetGeneration(1)
	evidence := restoreRequest("unused", "restore-uid", 1, "node").Object["status"].(map[string]interface{})
	evidence["verification"].(map[string]interface{})["sourceFenced"] = true
	evidence["verification"].(map[string]interface{})["operation"] = stringField(req.Object, "spec", "groupRestore", "operationUID")
	evidence["verification"].(map[string]interface{})["checkpointID"] = stringField(req.Object, "spec", "checkpointRef", "checkpointID")
	evidence["verification"].(map[string]interface{})["targetCluster"] = "aws"
	evidence["verification"].(map[string]interface{})["sourceFence"].(map[string]interface{})["operation"] = stringField(req.Object, "spec", "groupRestore", "operationUID")
	req.Object["status"] = evidence
	if err := r.Update(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyBatchCleanupValidatesAllUIDsBeforeDeletingAndRecordsAllSuccessors(t *testing.T) {
	for _, kind := range []string{"valid", "unverified", "bad-second-target", "bad-second-source", "bad-receipt"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, policy, input, intent := batchFixture(t, false)
			if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
				t.Fatal(err)
			}
			readyBatchTargets(t, r, intent, 3)
			r.GroupStage = groupStageRequest
			if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
				t.Fatal(err)
			}
			req := batchRequest(t, r, intent)
			markBatchVerified(t, r, req)
			switch kind {
			case "unverified":
				_ = unstructured.SetNestedField(req.Object, "Restoring", "status", "phase")
				if err := r.Update(ctx, req); err != nil {
					t.Fatal(err)
				}
			case "bad-second-target", "bad-second-source":
				name := intent.Batch[1].Replacement
				if kind == "bad-second-source" {
					name = intent.Batch[1].OldName
				}
				np := p.NewObject("NodeProvision")
				if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: name}, np); err != nil {
					t.Fatal(err)
				}
				np.SetUID("recreated")
				if err := r.Update(ctx, np); err != nil {
					t.Fatal(err)
				}
			case "bad-receipt":
				a := req.GetAnnotations()
				a[groupBatchPairsAnnotation] = "[]"
				req.SetAnnotations(a)
				if err := r.Update(ctx, req); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "valid" {
				successors, err := r.completedReplacementSuccessors(ctx, input)
				if err != nil || len(successors) != 3 {
					t.Fatalf("successors=%v err=%v", successors, err)
				}
				owned, err := r.ownedWorkerNodeProvisions(ctx, input, successors)
				if err != nil || len(owned) != 4 {
					t.Fatalf("owned=%v err=%v", owned, err)
				}
				for _, target := range intent.Batch {
					if owned[target.OldName].GetName() != target.Replacement {
						t.Fatal("old source overwrote verified successor")
					}
				}
				pending, err := r.pendingVerifiedGroupRetirement(ctx, input)
				if err != nil || pending == "" {
					t.Fatal("missing batch retirement gate")
				}
			}
			recovery := &GroupRecoveryReconciler{Client: r.Client, Reader: r.reader()}
			_, err := recovery.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(req)})
			if kind == "valid" || kind == "unverified" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("bad batch cleanup accepted")
			}
			for _, target := range intent.Batch {
				err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: target.OldName}, p.NewObject("NodeProvision"))
				if kind == "valid" {
					if !apierrors.IsNotFound(err) {
						t.Fatalf("source remains: %v", err)
					}
				} else if err != nil {
					t.Fatal("partially deleted batch before validating all participants")
				}
			}
		})
	}
}

func TestGroupBatchReceiptRejectsDuplicatesAndPrimaryMismatch(t *testing.T) {
	for _, kind := range []string{"valid", "duplicate", "wrong-primary", "emergency-owner", "empty-uid"} {
		t.Run(kind, func(t *testing.T) {
			req := newRestoreRequest()
			pairs := []groupReplacementPair{{"old1", "uid1", "new1", "newuid1"}, {"old2", "uid2", "new2", "newuid2"}}
			a := map[string]string{groupOldName: "old1", groupOldUID: "uid1", groupNewName: "new1", groupNewUID: "newuid1"}
			switch kind {
			case "duplicate":
				pairs[1] = pairs[0]
			case "wrong-primary":
				pairs[0].OldUID = "other"
			case "emergency-owner":
				a[groupCleanupOwner] = "owner"
			case "empty-uid":
				pairs[1].NewUID = ""
			}
			raw, _ := json.Marshal(pairs)
			a[groupBatchPairsAnnotation] = string(raw)
			req.SetAnnotations(a)
			_, err := groupReplacementPairs(req)
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s err=%v", kind, err)
			}
		})
	}
}

func TestGroupBatchSizeIsExplicitAndBounded(t *testing.T) {
	for _, raw := range []string{"", "1", "2", "3", "4", "0", "5", "x"} {
		t.Run(raw, func(t *testing.T) {
			p := p.NewObject("NodeRequest")
			p.SetAnnotations(map[string]string{groupBatchSizeAnnotation: raw})
			n, err := groupBatchSize(p, 4)
			wantValid := raw == "" || strings.Contains("1234", raw)
			if (err == nil) != wantValid || (raw == "" && n != 1) {
				t.Fatalf("n=%d err=%v", n, err)
			}
		})
	}
}

func TestPolicyBatchDispatchAndRestartKeepOnePinnedOperation(t *testing.T) {
	ctx := context.Background()
	r, policy, input, intent := batchFixture(t, false)
	r.GroupStage = groupStageIntent
	decision := p.Decision{DesiredWorkers: 4, OnDemandWorkers: 4}
	result, err := r.checkCapacityLifecycle(ctx, policy, input, decision)
	if err != nil || !result.Blocked || !strings.Contains(result.Message, "batch prepares 3") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	list := p.NewList("NodeProvision")
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 4 {
		t.Fatal("policy writer bypassed dedicated capacity stage")
	}
	// Restart by using new reconciler instances and only the persisted NodeRequest intent.
	capacity := &GroupCapacityReconciler{&PolicyReconciler{Client: r.Client, APIReader: r.reader()}}
	key := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
	for i := 0; i < 2; i++ {
		if _, err := capacity.Reconcile(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 7 {
		t.Fatalf("restart inventory=%d", len(list.Items))
	}
	readyBatchTargets(t, r, intent, 3)
	request := &GroupRequestReconciler{&PolicyReconciler{Client: r.Client, APIReader: r.reader()}}
	if _, err := request.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	req := batchRequest(t, r, intent)
	// A changed decision must not mutate the in-flight batch or allocate more VMs.
	fresh := p.NewObject("NodeRequest")
	if err := r.Get(ctx, key.NamespacedName, fresh); err != nil {
		t.Fatal(err)
	}
	raw := fresh.GetAnnotations()[groupOperationAnnotation]
	_, _ = r.checkCapacityLifecycle(ctx, fresh, p.ReadPolicyInput(fresh), p.Decision{DesiredWorkers: 4, OnDemandWorkers: 2})
	after := p.NewObject("NodeRequest")
	if err := r.Get(ctx, key.NamespacedName, after); err != nil {
		t.Fatal(err)
	}
	if after.GetAnnotations()[groupOperationAnnotation] != raw {
		t.Fatal("changed risk mutated operation")
	}
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 7 {
		t.Fatal("risk update allocated a second batch")
	}
	if _, err := request.Reconcile(ctx, key); err != nil {
		t.Fatal(err)
	}
	got := batchRequest(t, r, intent)
	if got.GetResourceVersion() != req.GetResourceVersion() {
		t.Fatal("request writer changed existing batch")
	}
}

func TestPolicyBatchBoundTwoDoesNotAllocateThirdTarget(t *testing.T) {
	ctx := context.Background()
	r, policy, input, _ := batchFixture(t, false)
	a := policy.GetAnnotations()
	a[groupBatchSizeAnnotation] = "2"
	policy.SetAnnotations(a)
	if err := r.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	r.GroupStage = groupStageIntent
	if _, err := r.checkCapacityLifecycle(ctx, policy, input, p.Decision{DesiredWorkers: 4, OnDemandWorkers: 4}); err != nil {
		t.Fatal(err)
	}
	capacity := &GroupCapacityReconciler{&PolicyReconciler{Client: r.Client, APIReader: r.reader()}}
	if _, err := capacity.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	list := p.NewList("NodeProvision")
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 6 {
		t.Fatalf("bounded batch inventory=%d", len(list.Items))
	}
}

func TestTwoRankBatchUsesTwoNewWorkersAndOneFullRestoreBothDirections(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			ctx := context.Background()
			r, policy, input, _ := batchFixture(t, reverse)
			input.TargetWorkers = 2
			a := policy.GetAnnotations()
			a[groupBatchSizeAnnotation] = "2"
			a[groupIntentAnnotation] = "old-0"
			policy.SetAnnotations(a)
			_ = unstructured.SetNestedField(policy.Object, int64(2), "spec", "targetWorkers")
			_ = unstructured.SetNestedField(policy.Object, "old-0", "status", "checkpoint", "replacementOperation")
			if err := r.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			_ = unstructured.SetNestedField(policy.Object, "old-0", "status", "checkpoint", "replacementOperation")
			if err := r.Status().Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			cp := p.NewObject("FluidCRMigration")
			if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "round"}, cp); err != nil {
				t.Fatal(err)
			}
			groupReport(cp)["pods"] = groupReport(cp)["pods"].([]interface{})[:2]
			if err := r.Update(ctx, cp); err != nil {
				t.Fatal(err)
			}
			rt := p.NewObject("TrainingRuntime")
			if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "runtime"}, rt); err != nil {
				t.Fatal(err)
			}
			status := rt.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})["status"].(map[string]interface{})
			status["sourcePods"] = status["sourcePods"].([]interface{})[:2]
			if err := r.Update(ctx, rt); err != nil {
				t.Fatal(err)
			}
			intent := groupOperation{PolicyUID: "policy", PolicyGeneration: 1}
			for rank := 0; rank < 4; rank++ {
				name := fmt.Sprintf("policy-worker-%02d", rank)
				np := p.NewObject("NodeProvision")
				if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: name}, np); err != nil {
					t.Fatal(err)
				}
				if rank >= 2 {
					if err := r.Delete(ctx, np); err != nil {
						t.Fatal(err)
					}
					continue
				}
				oldMarket, newMarket := "Spot", "OnDemand"
				if reverse {
					oldMarket, newMarket = "OnDemand", "Spot"
				}
				_ = unstructured.SetNestedField(np.Object, oldMarket, "spec", "marketType")
				if err := r.Update(ctx, np); err != nil {
					t.Fatal(err)
				}
				intent.Batch = append(intent.Batch, groupBatchTarget{name, string(np.GetUID()), replacementNodeProvisionName(name, string(np.GetUID())), newMarket})
			}
			first := intent.Batch[0]
			intent.OldName, intent.OldUID, intent.Replacement, intent.Market = first.OldName, first.OldUID, first.Replacement, first.Market
			intent.Operation = replacementOperationName(first.OldName, first.OldUID)
			if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
				t.Fatal(err)
			}
			list := p.NewList("NodeProvision")
			if err := r.List(ctx, list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 4 {
				t.Fatalf("two-rank overlap=%d, want2+2", len(list.Items))
			}
			readyBatchTargets(t, r, intent, 2)
			r.GroupStage = groupStageRequest
			if created, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil || !created {
				t.Fatalf("created=%v err=%v", created, err)
			}
			req := batchRequest(t, r, intent)
			pods, _, _ := unstructured.NestedSlice(req.Object, "spec", "pods")
			if intField(req.Object, "spec", "groupRestore", "worldSize") != 2 || len(pods) != 2 {
				t.Fatal("two-rank world shape changed")
			}
			for rank, raw := range pods {
				if stringField(raw.(map[string]interface{}), "targetNode") != fmt.Sprintf("new-node-%d", rank) {
					t.Fatal("two-rank destination map missing")
				}
			}
			if created, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil || created {
				t.Fatal("two-rank retry duplicated request")
			}
		})
	}
}

func TestPolicyBatchRetirementWaitsForEverySourceIncludingNonPrimary(t *testing.T) {
	ctx := context.Background()
	r, policy, input, intent := batchFixture(t, false)
	if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
		t.Fatal(err)
	}
	readyBatchTargets(t, r, intent, 3)
	r.GroupStage = groupStageRequest
	if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
		t.Fatal(err)
	}
	req := batchRequest(t, r, intent)
	markBatchVerified(t, r, req)
	// The primary may have completed its finalizer while a later pair is still terminating.
	first := p.NewObject("NodeProvision")
	first.SetNamespace("demo")
	first.SetName(intent.Batch[0].OldName)
	if err := r.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	pending, err := r.pendingVerifiedGroupRetirement(ctx, input)
	if err != nil || pending != intent.Batch[1].OldName {
		t.Fatalf("pending=%s err=%v", pending, err)
	}
}
