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
