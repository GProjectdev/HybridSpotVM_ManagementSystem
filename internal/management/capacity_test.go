package management

import (
	"context"
	"strings"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
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
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(testScheme()).WithRuntimeObjects(recovery).Build()

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
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(testScheme()).WithRuntimeObjects(recovery).Build()

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
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(testScheme()).WithRuntimeObjects(extra).Build()

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
	policy.Object["status"] = map[string]interface{}{
		trainingpolicy.StatusPolicyPath: map[string]interface{}{"desiredWorkers": int64(1)},
	}
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
 if err:=reconciler.Get(context.Background(),types.NamespacedName{Namespace:"default",Name:capacity.OperationName},op);err==nil{t.Fatal("automatic partial operation created without full checkpoint")}
 if !capacity.Blocked{t.Fatal("missing checkpoint did not block replacement")}
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
