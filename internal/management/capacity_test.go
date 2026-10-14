package management

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCapacityGateBlocksRetiredGeneratedOriginal(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 2)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	recovery := spotRecoveryFixture("recover-worker-00", "policy-uid", "train-worker-00", "old-worker-uid", "Completed")
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"), recovery)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile: %v", err)
	}

	list := trainingpolicy.NewList("NodeProvision")
	if err := reconciler.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("list node provisions: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("node provisions = %d, want none when retired generated slot is present", len(list.Items))
	}
	assertPolicyReason(t, reconciler.Client, "retired_generated_slot")
}

func TestCapacityGateUsesFreshAPIReaderForRetirement(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 1)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	recovery := spotRecoveryFixture("recover-worker-00", "policy-uid", "train-worker-00", "old-worker-uid", "Pending")
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"))
	reconciler.APIReader = freshPolicyReader(t, reconciler, recovery)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile: %v", err)
	}

	created := trainingpolicy.NewObject("NodeProvision")
	err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-00"}, created)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("generated slot get error = %v, want not found because APIReader saw retirement marker", err)
	}
	assertPolicyReason(t, reconciler.Client, "retired_generated_slot")
}

func TestCapacityGateCleanupRequestedRetiresSlotAfterRestart(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 1)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	recovery := spotRecoveryFixture("recover-worker-00", "policy-uid", "train-worker-00", "old-worker-uid", "CleanupRequested")
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"))
	reconciler.APIReader = freshPolicyReader(t, reconciler, recovery)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile after restart: %v", err)
	}

	created := trainingpolicy.NewObject("NodeProvision")
	err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-00"}, created)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("generated slot get error = %v, want not found because CleanupRequested recovery retires slot", err)
	}
	assertPolicyReason(t, reconciler.Client, "retired_generated_slot")
}

func TestCapacityGateRejectedRecoveryStillRetiresGeneratedSlot(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 1)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	recovery := spotRecoveryFixture("recover-worker-00", "policy-uid", "train-worker-00", "old-worker-uid", "Rejected")
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"), recovery)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile: %v", err)
	}

	created := trainingpolicy.NewObject("NodeProvision")
	err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-00"}, created)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("generated slot get error = %v, want not found because immutable recovery request retires slot", err)
	}
	assertPolicyReason(t, reconciler.Client, "retired_generated_slot")
}

func TestCapacityGateExtraOwnedInventoryPreventsNewSlot(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 1)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	extra := trainingpolicy.NewObject("NodeProvision")
	extra.SetNamespace("default")
	extra.SetName("train-worker-extra")
	extra.SetLabels(map[string]string{
		trainingpolicy.LabelPolicyUID: "policy-uid",
		trainingpolicy.LabelRole:      "worker",
	})
	extra.Object["spec"] = map[string]interface{}{"marketType": "Spot"}
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"), extra)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile: %v", err)
	}

	missing := trainingpolicy.NewObject("NodeProvision")
	err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-00"}, missing)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("generated slot get error = %v, want not found", err)
	}
	assertPolicyReason(t, reconciler.Client, "capacity_inventory_full")
}

func TestCapacityGateProjectedInventoryBlocksUnexpectedWorkerAndMissingGeneratedSlots(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 2)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	extra := trainingpolicy.NewObject("NodeProvision")
	extra.SetNamespace("default")
	extra.SetName("train-worker-extra")
	extra.SetLabels(map[string]string{
		trainingpolicy.LabelPolicyUID: "policy-uid",
		trainingpolicy.LabelRole:      "worker",
	})
	extra.Object["spec"] = map[string]interface{}{"marketType": "Spot"}
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"))
	reconciler.APIReader = freshPolicyReader(t, reconciler, extra)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile: %v", err)
	}

	list := trainingpolicy.NewList("NodeProvision")
	if err := reconciler.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("list cached node provisions: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("cached node provisions = %d, want no new generated workers", len(list.Items))
	}
	assertPolicyReason(t, reconciler.Client, "capacity_inventory_full")
}

func TestCapacityGateFixedTargetChangeKeepsHistoricalBaselineAcrossReconciles(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 2)
	_ = unstructured.SetNestedField(policy.Object, int64(1), "status", trainingpolicy.StatusPolicyPath, "desiredWorkers")
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}

	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("policy reconcile %d: %v", i+1, err)
		}
		updated := trainingpolicy.NewObject("TrainingPolicy")
		if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train"}, updated); err != nil {
			t.Fatalf("get policy after reconcile %d: %v", i+1, err)
		}
		desired, _, _ := unstructured.NestedInt64(updated.Object, "status", trainingpolicy.StatusPolicyPath, "desiredWorkers")
		if desired != 1 {
			t.Fatalf("desiredWorkers after reconcile %d = %d, want historical 1", i+1, desired)
		}
		assertPolicyReason(t, reconciler.Client, "fixed_target_changed")
	}
}

func TestCapacityLifecycleRequiresGroupRoundForOnDemandToSpot(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 1)
	policy.Object["spec"].(map[string]interface{})["replacement"] = map[string]interface{}{"enabled": true}
	runtimeObj := runtimeFixtureWithPods(now, []interface{}{
		map[string]interface{}{"name": "trainer-0", "uid": "survivor-pod-uid", "rank": int64(0), "nodeName": "survivor-node"},
		map[string]interface{}{"name": "trainer-1", "uid": "target-pod-uid", "rank": int64(1), "nodeName": "train-worker-00"},
	})
	oldNP := trainingpolicy.NewObject("NodeProvision")
	oldNP.SetNamespace("default")
	oldNP.SetName("train-worker-00")
	oldNP.SetUID(types.UID("old-worker-uid"))
	oldNP.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy-uid", trainingpolicy.LabelRole: "worker"})
	oldNP.Object["spec"] = map[string]interface{}{"marketType": "OnDemand", "hostname": "train-worker-00"}
	oldNP.Object["status"] = map[string]interface{}{"nodeName": "train-worker-00", "instanceId": "i-old"}
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, workloadFixture("workload-uid"), oldNP)
	input := trainingpolicy.ReadPolicySpec(policy)
	decision := trainingpolicy.Decision{DesiredWorkers: 1, OnDemandWorkers: 0}

	capacity, err := reconciler.checkCapacityLifecycle(context.Background(), policy, input, decision)
	if err != nil {
		t.Fatalf("checkCapacityLifecycle: %v", err)
	}
	if !capacity.ReplacementRequired || capacity.OperationName == "" || capacity.ReplacementName == "" {
		t.Fatalf("capacity decision = %#v, want replacement operation", capacity)
	}
	op := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: capacity.OperationName}, op); err == nil {
		t.Fatal("automatic partial operation created without full checkpoint")
	}
	if !capacity.Blocked {
		t.Fatal("missing checkpoint did not block replacement")
	}
}

func TestCapacityPreparesAllMismatchInstancesButCreatesOneOperation(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 3)
	_ = unstructured.SetNestedField(policy.Object, true, "spec", "replacement", "enabled")
	_ = unstructured.SetNestedField(policy.Object, "aws", "spec", "sourceCluster")
	runtimeObj := runtimeFixtureForParallelReplacement(now)
	workload := workloadFixture("workload-uid")
	attachCheckpointVolume(workload)
	input := trainingpolicy.ReadPolicySpec(policy)
	old0 := capacityWorkerFixture(input, 0, "Spot", "old-00-uid")
	old1 := capacityWorkerFixture(input, 1, "OnDemand", "old-01-uid")
	old2 := capacityWorkerFixture(input, 2, "Spot", "old-02-uid")
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, workload, old0, old1, old2)

	capacity, err := reconciler.checkCapacityLifecycle(context.Background(), policy, input, trainingpolicy.Decision{DesiredWorkers: 3, OnDemandWorkers: 1})
	if err != nil {
		t.Fatalf("checkCapacityLifecycle: %v", err)
	}
	if !capacity.Blocked || !capacity.ReplacementRequired || capacity.Reason != "replacement_required" {
		t.Fatalf("capacity decision = %#v, want replacement_required block", capacity)
	}
	if capacity.OperationName != replacementOperationName("train-worker-00", "old-00-uid") {
		t.Fatalf("operation = %q, want first serial replacement", capacity.OperationName)
	}
	replacements := trainingpolicy.NewList("NodeProvision")
	if err := reconciler.List(context.Background(), replacements, client.InNamespace("default"), client.MatchingLabels{trainingpolicy.LabelRole: "replacement"}); err != nil {
		t.Fatalf("list replacements: %v", err)
	}
	if len(replacements.Items) != 2 {
		t.Fatalf("replacement NodeProvision count = %d, want 2", len(replacements.Items))
	}
	wantMarkets := map[string]string{
		replacementNodeProvisionName("train-worker-00", "old-00-uid"): "OnDemand",
		replacementNodeProvisionName("train-worker-01", "old-01-uid"): "Spot",
	}
	for i := range replacements.Items {
		item := &replacements.Items[i]
		if market := stringField(item.Object, "spec", "marketType"); market != wantMarkets[item.GetName()] {
			t.Fatalf("replacement %s market = %q, want %q", item.GetName(), market, wantMarkets[item.GetName()])
		}
		if keyPair := stringField(item.Object, "spec", "awsConfig", "keyPairName"); keyPair != "" {
			t.Fatalf("replacement %s inherited keyPairName %q", item.GetName(), keyPair)
		}
	}
	operations := newSpotReplacementList()
	if err := reconciler.List(context.Background(), operations, client.InNamespace("default")); err != nil {
		t.Fatalf("list SpotReplacement operations: %v", err)
	}
	if len(operations.Items) != 1 || operations.Items[0].GetName() != capacity.OperationName {
		t.Fatalf("SpotReplacement operations = %#v, want only %s", operations.Items, capacity.OperationName)
	}
}

func TestCapacityPrewarmRepeatedReconcileIsIdempotent(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	reconciler, policy, input := parallelReplacementCapacityFixture(t, now)
	decision := trainingpolicy.Decision{DesiredWorkers: 3, OnDemandWorkers: 1}

	for i := 0; i < 2; i++ {
		capacity, err := reconciler.checkCapacityLifecycle(context.Background(), policy, input, decision)
		if err != nil {
			t.Fatalf("checkCapacityLifecycle %d: %v", i+1, err)
		}
		if capacity.Reason != "replacement_required" || capacity.OperationName != replacementOperationName("train-worker-00", "old-00-uid") {
			t.Fatalf("capacity decision %d = %#v", i+1, capacity)
		}
		assertReplacementInventory(t, reconciler.Client, 2, 1)
		markAllReplacementNodeUIDs(t, reconciler.Client)
	}
}

func TestCapacitySecondOperationUsesFreshSnapshotsAfterFirstCompletes(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	reconciler, policy, input := parallelReplacementCapacityFixture(t, now)
	decision := trainingpolicy.Decision{DesiredWorkers: 3, OnDemandWorkers: 1}

	first, err := reconciler.checkCapacityLifecycle(context.Background(), policy, input, decision)
	if err != nil {
		t.Fatalf("initial checkCapacityLifecycle: %v", err)
	}
	if first.OperationName != replacementOperationName("train-worker-00", "old-00-uid") {
		t.Fatalf("first operation = %q", first.OperationName)
	}
	markAllReplacementNodeUIDs(t, reconciler.Client)
	markSpotReplacementCompleted(t, reconciler.Client, first.OperationName)
	firstReplacementName := replacementNodeProvisionName("train-worker-00", "old-00-uid")
	markReplacementNodeUID(t, reconciler.Client, firstReplacementName, "replacement-00-uid")
	replacementNode := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: firstReplacementName}, replacementNode); err != nil {
		t.Fatal(err)
	}
	replacementNode.Object["status"] = map[string]interface{}{
		"phase": "Ready", "nodeName": firstReplacementName + "-node",
		"memberUID": "replacement-00-member-uid", "instanceId": "i-replacement-00",
	}
	if err := reconciler.Update(context.Background(), replacementNode); err != nil {
		t.Fatal(err)
	}
	recovery := spotRecoveryFixture(first.OperationName+"-cleanup", "policy-uid", "train-worker-00", "old-00-uid", "Completed")
	recovery.Object["spec"].(map[string]interface{})["replacementNodeProvisionRef"] = map[string]interface{}{"name": firstReplacementName, "uid": "replacement-00-uid"}
	if err := reconciler.Create(context.Background(), recovery); err != nil {
		t.Fatalf("create completed recovery: %v", err)
	}
	original := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "train-worker-00"}, original); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Delete(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	refreshParallelRuntimePod(t, reconciler.Client, "trainer-0", "pod-0-restored", firstReplacementName+"-node")
	refreshParallelRuntimePod(t, reconciler.Client, "trainer-1", "pod-1-fresh", "train-worker-01-node")

	second, err := reconciler.checkCapacityLifecycle(context.Background(), policy, input, decision)
	if err != nil {
		t.Fatalf("second checkCapacityLifecycle: %v", err)
	}
	wantOperation := replacementOperationName("train-worker-01", "old-01-uid")
	if second.OperationName != wantOperation {
		t.Fatalf("second operation = %q, want %q", second.OperationName, wantOperation)
	}
	op := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: wantOperation}, op); err != nil {
		t.Fatalf("get second operation: %v; decision=%#v", err, second)
	}
	pods, _, _ := unstructured.NestedSlice(op.Object, "spec", "pods")
	if len(pods) != 1 || stringField(pods[0].(map[string]interface{}), "sourcePodUID") != "pod-1-fresh" {
		t.Fatalf("second operation pods = %#v, want fresh target PodUID", pods)
	}
	survivors, _, _ := unstructured.NestedSlice(op.Object, "spec", "partialRestore", "preservedSurvivors")
	if len(survivors) != 2 {
		t.Fatalf("second operation survivors = %#v, want two fresh survivor snapshots", survivors)
	}
	foundRestoredRank0 := false
	for _, raw := range survivors {
		survivor := raw.(map[string]interface{})
		if intField(survivor, "rank") == 0 {
			foundRestoredRank0 = true
			if stringField(survivor, "podUID") != "pod-0-restored" || stringField(survivor, "nodeName") != firstReplacementName+"-node" {
				t.Fatalf("rank 0 survivor = %#v, want restored pod on first replacement", survivor)
			}
		}
		if stringField(survivor, "podUID") == "pod-0-uid" {
			t.Fatalf("second operation survivors = %#v, still contains initial rank 0 UID", survivors)
		}
	}
	if !foundRestoredRank0 {
		t.Fatalf("second operation survivors = %#v, want restored rank 0 survivor", survivors)
	}
	assertReplacementInventory(t, reconciler.Client, 2, 2)
}

func TestCapacityPrewarmRefusesUnsafeSourcesBeforeCreate(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mutatePolicy    func(*unstructured.Unstructured)
		mutateRuntime   func(*unstructured.Unstructured, time.Time)
		mutateSource    func(*unstructured.Unstructured)
		wantReason      string
		wantNoOperation bool
	}{
		{name: "deleting source", mutateSource: func(np *unstructured.Unstructured) {
			ts := metav1.Now()
			np.SetDeletionTimestamp(&ts)
			np.SetFinalizers([]string{"cleanup"})
		}, wantReason: "replacement_unsupported", wantNoOperation: true},
		{name: "spec fenced source", mutateSource: func(np *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(np.Object, map[string]interface{}{"operation": "other"}, "spec", "fence")
		}, wantReason: "replacement_unsupported", wantNoOperation: true},
		{name: "status fenced source", mutateSource: func(np *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(np.Object, map[string]interface{}{"phase": "Fenced"}, "status", "fence")
		}, wantReason: "replacement_unsupported", wantNoOperation: true},
		{name: "not ready source", mutateSource: func(np *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(np.Object, "Pending", "status", "phase")
		}, wantReason: "replacement_unsupported", wantNoOperation: true},
		{name: "stale runtime", mutateRuntime: func(rt *unstructured.Unstructured, now time.Time) {
			setParallelRuntimeObservedAt(rt, now.Add(-3*time.Minute))
		}, wantReason: "replacement_unsupported", wantNoOperation: true},
		{name: "planned partial disabled", mutatePolicy: func(policy *unstructured.Unstructured) {
			policy.SetAnnotations(map[string]string{"training.dcnlab.com/planned-partial": "disabled"})
		}, wantReason: "replacement_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := mustParseTime(t, "2026-09-26T00:00:00Z")
			policy := policyFixtureForCapacity(now, 3)
			_ = unstructured.SetNestedField(policy.Object, true, "spec", "replacement", "enabled")
			_ = unstructured.SetNestedField(policy.Object, "aws", "spec", "sourceCluster")
			runtimeObj := runtimeFixtureForParallelReplacement(now)
			workload := workloadFixture("workload-uid")
			attachCheckpointVolume(workload)
			input := trainingpolicy.ReadPolicySpec(policy)
			old0 := capacityWorkerFixture(input, 0, "Spot", "old-00-uid")
			old1 := capacityWorkerFixture(input, 1, "OnDemand", "old-01-uid")
			old2 := capacityWorkerFixture(input, 2, "Spot", "old-02-uid")
			if tc.mutatePolicy != nil {
				tc.mutatePolicy(policy)
			}
			if tc.mutateRuntime != nil {
				tc.mutateRuntime(runtimeObj, now)
			}
			if tc.mutateSource != nil {
				tc.mutateSource(old0)
			}
			reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, workload, old0, old1, old2)
			if tc.name == "deleting source" {
				source := trainingpolicy.NewObject("NodeProvision")
				key := client.ObjectKey{Namespace: input.Namespace, Name: old0.GetName()}
				if err := reconciler.Get(context.Background(), key, source); err != nil {
					t.Fatal(err)
				}
				if err := reconciler.Delete(context.Background(), source); err != nil {
					t.Fatal(err)
				}
				if err := reconciler.Get(context.Background(), key, source); err != nil || source.GetDeletionTimestamp().IsZero() {
					t.Fatalf("fixture must expose deleting source: %v", err)
				}
			}

			capacity, err := reconciler.checkCapacityLifecycle(context.Background(), policy, input, trainingpolicy.Decision{DesiredWorkers: 3, OnDemandWorkers: 1})
			if err != nil {
				t.Fatalf("checkCapacityLifecycle: %v", err)
			}
			if capacity.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q; decision=%#v", capacity.Reason, tc.wantReason, capacity)
			}
			assertReplacementInventory(t, reconciler.Client, 0, 0)
			if tc.wantNoOperation {
				assertSpotReplacementCount(t, reconciler.Client, 0)
			}
		})
	}
}

func TestCompletedReplacementSuccessorsFollowsLatestReplacementChain(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 1)
	first := spotRecoveryFixture("first", "policy-uid", "train-worker-00", "old-worker-uid", "Completed")
	first.Object["spec"].(map[string]interface{})["replacementNodeProvisionRef"] = map[string]interface{}{"name": "train-worker-00-old-worker-u-replacement", "uid": "first-uid"}
	second := spotRecoveryFixture("second", "policy-uid", "train-worker-00-old-worker-u-replacement", "first-uid", "Completed")
	second.Object["spec"].(map[string]interface{})["replacementNodeProvisionRef"] = map[string]interface{}{"name": "train-worker-00-first-uid-replacement", "uid": "second-uid"}
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, first, second)

	successors, err := reconciler.completedReplacementSuccessors(context.Background(), trainingpolicy.ReadPolicySpec(policy))
	if err != nil {
		t.Fatalf("completedReplacementSuccessors: %v", err)
	}
	if got := successors["train-worker-00"]; got != "train-worker-00-first-uid-replacement" {
		t.Fatalf("successor = %q, want latest replacement", got)
	}
	if len(successors) != 1 {
		t.Fatalf("intermediate replacements must not become logical slots: %v", successors)
	}
}

func TestCompletedReplacementSuccessorsRejectsRecreatedIntermediate(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := policyFixtureForCapacity(now, 1)
	first := spotRecoveryFixture("first", "policy-uid", "train-worker-00", "old", "Completed")
	first.Object["spec"].(map[string]interface{})["replacementNodeProvisionRef"] = map[string]interface{}{"name": "middle", "uid": "original-middle"}
	second := spotRecoveryFixture("second", "policy-uid", "middle", "recreated-middle", "Completed")
	second.Object["spec"].(map[string]interface{})["replacementNodeProvisionRef"] = map[string]interface{}{"name": "last", "uid": "last-uid"}
	r := policyReconcilerFixture(t, func() time.Time { return now }, policy, first, second)
	if _, err := r.completedReplacementSuccessors(context.Background(), trainingpolicy.ReadPolicySpec(policy)); err == nil || !strings.Contains(err.Error(), "UID discontinuity") {
		t.Fatalf("expected UID discontinuity rejection, got %v", err)
	}
}

func TestNewNodeProvisionUsesGPUCapacityTemplate(t *testing.T) {
	input := trainingpolicy.PolicyInput{
		Namespace:  "default",
		PolicyName: "train",
		PolicyUID:  types.UID("policy-uid"),
		Capacity: trainingpolicy.CapacityDefaults{
			HardwareType: "gpu",
			NodeLabel:    "gpu",
			AWSCluster:   "aws",
		},
	}

	np := trainingpolicy.NewNodeProvision(input, 0, "Spot")
	nodeLabel, _, _ := unstructured.NestedString(np.Object, "spec", "nodeLabel")
	hardwareType, _, _ := unstructured.NestedString(np.Object, "spec", "hardwareType")
	if nodeLabel != "gpu" || hardwareType != "gpu" {
		t.Fatalf("nodeLabel/hardwareType = %q/%q, want gpu/gpu", nodeLabel, hardwareType)
	}
	if strings.Contains(nodeLabel, "training.dcnlab.com/policy") {
		t.Fatalf("nodeLabel carries policy metadata: %q", nodeLabel)
	}
	if got := np.GetLabels()[trainingpolicy.LabelPolicy]; got != "train" {
		t.Fatalf("policy label = %q, want train", got)
	}
}

func policyFixtureForCapacity(now time.Time, targetWorkers int64) *unstructured.Unstructured {
	policy := checkpointPolicyFixture(now)
	policy.Object["spec"].(map[string]interface{})["targetWorkers"] = targetWorkers
	policy.Object["spec"].(map[string]interface{})["capacity"] = map[string]interface{}{
		"aws": map[string]interface{}{"karmadaCluster": "aws", "hardwareType": "gpu", "nodeLabel": "gpu"},
	}
	return policy
}

func parallelReplacementCapacityFixture(t *testing.T, now time.Time) (*PolicyReconciler, *unstructured.Unstructured, trainingpolicy.PolicyInput) {
	t.Helper()
	policy := policyFixtureForCapacity(now, 3)
	_ = unstructured.SetNestedField(policy.Object, true, "spec", "replacement", "enabled")
	_ = unstructured.SetNestedField(policy.Object, "aws", "spec", "sourceCluster")
	runtimeObj := runtimeFixtureForParallelReplacement(now)
	workload := workloadFixture("workload-uid")
	attachCheckpointVolume(workload)
	input := trainingpolicy.ReadPolicySpec(policy)
	old0 := capacityWorkerFixture(input, 0, "Spot", "old-00-uid")
	old1 := capacityWorkerFixture(input, 1, "OnDemand", "old-01-uid")
	old2 := capacityWorkerFixture(input, 2, "Spot", "old-02-uid")
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, workload, old0, old1, old2)
	return reconciler, policy, input
}

func capacityWorkerFixture(input trainingpolicy.PolicyInput, ordinal int64, market, uid string) *unstructured.Unstructured {
	np := trainingpolicy.NewNodeProvision(input, ordinal, market)
	np.SetUID(types.UID(uid))
	nodeName := np.GetName() + "-node"
	np.Object["status"] = map[string]interface{}{
		"phase":      "Ready",
		"nodeName":   nodeName,
		"instanceId": "i-" + uid,
		"memberUID":  "member-" + uid,
	}
	spec := np.Object["spec"].(map[string]interface{})
	spec["hostname"] = np.GetName()
	spec["awsConfig"] = map[string]interface{}{"keyPairName": "nodeprovision-" + np.GetName()}
	return np
}

func runtimeFixtureForParallelReplacement(now time.Time) *unstructured.Unstructured {
	pods := []interface{}{}
	sourcePods := []interface{}{}
	for i := int64(0); i < 3; i++ {
		workerName := fmt.Sprintf("train-worker-%02d", i)
		podName := fmt.Sprintf("trainer-%d", i)
		pods = append(pods, map[string]interface{}{"name": podName, "uid": fmt.Sprintf("pod-%d-uid", i), "rank": i, "nodeName": workerName + "-node", "checkpointID": "ckpt-1", "observedAt": now.Format(time.RFC3339)})
		sourcePods = append(sourcePods, map[string]interface{}{"name": podName, "uid": fmt.Sprintf("pod-%d-uid", i), "rank": i, "nodeName": workerName + "-node"})
	}
	runtimeObj := runtimeFixtureWithPods(now, pods)
	runtimeObj.SetUID(types.UID("runtime-uid"))
	runtimeObj.Object["spec"].(map[string]interface{})["sourceCluster"] = "aws"
	cluster := runtimeObj.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})
	cluster["clusterName"] = "aws"
	status := cluster["status"].(map[string]interface{})
	status["sourceWorldUID"] = "workload-uid"
	status["sourcePods"] = sourcePods
	return runtimeObj
}

func attachCheckpointVolume(workload *unstructured.Unstructured) {
	workload.Object["spec"] = map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
		"containers": []interface{}{map[string]interface{}{"name": "trainer", "volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}}}},
		"volumes":    []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}}},
	}}}
}

func assertReplacementInventory(t *testing.T, c client.Client, wantNodes, wantOperations int) {
	t.Helper()
	replacements := trainingpolicy.NewList("NodeProvision")
	if err := c.List(context.Background(), replacements, client.InNamespace("default"), client.MatchingLabels{trainingpolicy.LabelRole: "replacement"}); err != nil {
		t.Fatalf("list replacement NodeProvisions: %v", err)
	}
	if len(replacements.Items) != wantNodes {
		t.Fatalf("replacement NodeProvision count = %d, want %d", len(replacements.Items), wantNodes)
	}
	assertSpotReplacementCount(t, c, wantOperations)
}

func assertSpotReplacementCount(t *testing.T, c client.Client, want int) {
	t.Helper()
	operations := newSpotReplacementList()
	if err := c.List(context.Background(), operations, client.InNamespace("default")); err != nil {
		t.Fatalf("list SpotReplacement operations: %v", err)
	}
	if len(operations.Items) != want {
		t.Fatalf("SpotReplacement operation count = %d, want %d", len(operations.Items), want)
	}
}

func markSpotReplacementCompleted(t *testing.T, c client.Client, name string) {
	t.Helper()
	op := newSpotReplacementObject()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, op); err != nil {
		t.Fatalf("get SpotReplacement %s: %v", name, err)
	}
	op.Object["status"] = map[string]interface{}{"phase": "Completed"}
	if err := c.Update(context.Background(), op); err != nil {
		t.Fatalf("complete SpotReplacement %s: %v", name, err)
	}
}

func markReplacementNodeUID(t *testing.T, c client.Client, name, uid string) {
	t.Helper()
	np := trainingpolicy.NewObject("NodeProvision")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, np); err != nil {
		t.Fatalf("get replacement NodeProvision %s: %v", name, err)
	}
	np.SetUID(types.UID(uid))
	if err := c.Update(context.Background(), np); err != nil {
		t.Fatalf("set replacement NodeProvision UID: %v", err)
	}
}

func markAllReplacementNodeUIDs(t *testing.T, c client.Client) {
	t.Helper()
	list := trainingpolicy.NewList("NodeProvision")
	if err := c.List(context.Background(), list, client.InNamespace("default"), client.MatchingLabels{trainingpolicy.LabelRole: "replacement"}); err != nil {
		t.Fatalf("list replacement NodeProvisions: %v", err)
	}
	for i := range list.Items {
		item := list.Items[i].DeepCopy()
		if item.GetUID() != "" {
			continue
		}
		item.SetUID(types.UID(item.GetName() + "-uid"))
		if err := c.Update(context.Background(), item); err != nil {
			t.Fatalf("set replacement NodeProvision %s UID: %v", item.GetName(), err)
		}
	}
}

func refreshParallelRuntimePod(t *testing.T, c client.Client, podName, podUID, nodeName string) {
	t.Helper()
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-runtime"}, runtimeObj); err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	cluster := runtimeObj.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})
	status := cluster["status"].(map[string]interface{})
	for _, raw := range status["pods"].([]interface{}) {
		pod := raw.(map[string]interface{})
		if stringField(pod, "name") == podName {
			pod["uid"] = podUID
			pod["nodeName"] = nodeName
		}
	}
	for _, raw := range status["sourcePods"].([]interface{}) {
		pod := raw.(map[string]interface{})
		if stringField(pod, "name") == podName {
			pod["uid"] = podUID
			pod["nodeName"] = nodeName
		}
	}
	if err := c.Update(context.Background(), runtimeObj); err != nil {
		t.Fatalf("update runtime pod UID: %v", err)
	}
}

func setParallelRuntimeObservedAt(runtimeObj *unstructured.Unstructured, observed time.Time) {
	cluster := runtimeObj.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})
	status := cluster["status"].(map[string]interface{})
	status["observedAt"] = observed.Format(time.RFC3339)
	for _, raw := range status["pods"].([]interface{}) {
		raw.(map[string]interface{})["observedAt"] = observed.Format(time.RFC3339)
	}
}

func freshPolicyReader(t *testing.T, r *PolicyReconciler, extra *unstructured.Unstructured) client.Reader {
	t.Helper()
	var objects []client.Object
	for _, kind := range []string{"StatefulSet", "TrainingPolicy"} {
		list := trainingpolicy.NewList(kind)
		if err := r.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		for i := range list.Items {
			objects = append(objects, list.Items[i].DeepCopy())
		}
	}
	bindings := &unstructured.UnstructuredList{}
	bindings.SetGroupVersionKind(bindingObject().GroupVersionKind().GroupVersion().WithKind("ResourceBindingList"))
	if err := r.List(context.Background(), bindings); err != nil {
		t.Fatal(err)
	}
	for i := range bindings.Items {
		objects = append(objects, bindings.Items[i].DeepCopy())
	}
	objects = append(objects, extra)
	return fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
}

func spotRecoveryFixture(name, policyUID, oldName, oldUID, phase string) *unstructured.Unstructured {
	recovery := trainingpolicy.NewObject("SpotRecovery")
	recovery.SetNamespace("default")
	recovery.SetName(name)
	recovery.Object["spec"] = map[string]interface{}{
		"policyRef":           map[string]interface{}{"name": "train", "uid": policyUID, "generation": int64(1)},
		"oldNodeProvisionRef": map[string]interface{}{"name": oldName, "uid": oldUID},
	}
	recovery.Object["status"] = map[string]interface{}{"phase": phase, "observedGeneration": int64(1)}
	return recovery
}

func assertPolicyReason(t *testing.T, c client.Client, want string) {
	t.Helper()
	updated := trainingpolicy.NewObject("TrainingPolicy")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train"}, updated); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	reason, _, _ := unstructured.NestedString(updated.Object, "status", trainingpolicy.StatusPolicyPath, "reason")
	if reason != want {
		t.Fatalf("policy reason = %q, want %q", reason, want)
	}
}
