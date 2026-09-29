package management

import (
	"context"
	"strings"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStatusPatchCarriesUIDAndResourceVersionPreconditions(t *testing.T) {
	policy := trainingpolicy.NewObject("TrainingPolicy")
	policy.SetUID(types.UID("policy-uid"))
	policy.SetResourceVersion("42")
	patch := statusSubtreePatch(policy, trainingpolicy.StatusPolicyPath, map[string]interface{}{"reason": "test"})
	metadata := patch["metadata"].(map[string]interface{})
	if metadata["uid"] != "policy-uid" || metadata["resourceVersion"] != "42" {
		t.Fatalf("metadata preconditions = %#v, want uid/resourceVersion", metadata)
	}
}

func TestPolicyCreateIfMissingRejectsForeignOwner(t *testing.T) {
	existing := trainingpolicy.NewObject("NodeProvision")
	existing.SetNamespace("default")
	existing.SetName("train-worker-00")
	existing.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "other-policy"})
	existing.Object["spec"] = map[string]interface{}{"marketType": "Spot"}

	desired := trainingpolicy.NewObject("NodeProvision")
	desired.SetNamespace("default")
	desired.SetName("train-worker-00")
	desired.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "this-policy"})
	desired.Object["spec"] = map[string]interface{}{"marketType": "Spot"}

	reconciler := &PolicyReconciler{Client: fake.NewClientBuilder().WithRuntimeObjects(existing).Build()}
	err := reconciler.createIfMissing(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "does not match desired policy uid") {
		t.Fatalf("error = %v, want foreign owner rejection", err)
	}
}

func TestPolicyCreateIfMissingRejectsUnlabeledExistingObject(t *testing.T) {
	existing := trainingpolicy.NewObject("NodeProvision")
	existing.SetNamespace("default")
	existing.SetName("train-worker-00")
	existing.Object["spec"] = map[string]interface{}{"marketType": "Spot"}

	desired := trainingpolicy.NewObject("NodeProvision")
	desired.SetNamespace("default")
	desired.SetName("train-worker-00")
	desired.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "this-policy"})
	desired.Object["spec"] = map[string]interface{}{"marketType": "Spot"}

	reconciler := &PolicyReconciler{Client: fake.NewClientBuilder().WithRuntimeObjects(existing).Build()}
	err := reconciler.createIfMissing(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "owner uid") {
		t.Fatalf("error = %v, want unlabeled object rejection", err)
	}
}

func TestPropagationPolicyBuilderLabelsAndDriftRejected(t *testing.T) {
	input := trainingpolicy.PolicyInput{Namespace: "default", PolicyName: "train", PolicyUID: "policy-uid"}
	target := trainingpolicy.NewObject("NodeProvision")
	target.SetNamespace("default")
	target.SetName("train-worker-00")
	desired := trainingpolicy.NewPropagationPolicyFor(input, target, "aws")
	if got := desired.GetLabels()[trainingpolicy.LabelPolicyUID]; got != "policy-uid" {
		t.Fatalf("pp owner uid = %q, want policy-uid", got)
	}
	existing := desired.DeepCopy()
	_ = unstructured.SetNestedStringMap(existing.Object, map[string]string{"unexpected": "cluster"}, "spec", "placement", "clusterAffinity", "clusterNames")
	reconciler := &PolicyReconciler{Client: fake.NewClientBuilder().WithRuntimeObjects(existing).Build()}
	err := reconciler.createIfMissing(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "placement drift") {
		t.Fatalf("error = %v, want placement drift rejection", err)
	}
}

func TestPolicyEnsureNodeProvisionsDoesNotResurrectRetiredSlot(t *testing.T) {
	input := trainingpolicy.PolicyInput{Namespace: "default", PolicyName: "train", PolicyUID: "policy-uid", TargetWorkers: 2, Capacity: trainingpolicy.CapacityDefaults{AWSCluster: "aws"}}
	recovery := trainingpolicy.NewObject("SpotRecovery")
	recovery.SetNamespace("default")
	recovery.SetName("recover-worker-00")
	recovery.SetGeneration(3)
	recovery.Object["spec"] = map[string]interface{}{
		"policyRef":             map[string]interface{}{"name": "train", "uid": "policy-uid"},
		"oldNodeProvisionRef":   map[string]interface{}{"name": "train-worker-00", "uid": "old-worker-uid"},
		"newNodeProvisionRef":   map[string]interface{}{"name": "train-worker-00-repl", "uid": "replacement-uid"},
		"restoreRequestRef":     map[string]interface{}{"uid": "restore-request-uid"},
		"verifiedCheckpointRef": map[string]interface{}{"checkpointID": "ckpt-1"},
	}
	recovery.Object["status"] = map[string]interface{}{}
	reconciler := &PolicyReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme()).WithRuntimeObjects(recovery).Build()}

	err := reconciler.ensureNodeProvisions(context.Background(), input, trainingpolicy.Decision{DesiredWorkers: 2, SpotWorkers: 2})
	if err != nil {
		t.Fatalf("ensure node provisions: %v", err)
	}
	oldSlot := trainingpolicy.NewObject("NodeProvision")
	err = reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-00"}, oldSlot)
	if err == nil {
		t.Fatal("retired train-worker-00 was recreated")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("get retired slot error = %v, want not found", err)
	}
	activeSlot := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-01"}, activeSlot); err != nil {
		t.Fatalf("expected non-retired slot: %v", err)
	}
}

func TestPolicyReconcileBlocksProvisioningWhenLiveWorkloadUIDStale(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	staleWorkload := workloadFixture("new-workload-uid")
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, staleWorkload)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile: %v", err)
	}
	list := trainingpolicy.NewList("NodeProvision")
	if err := reconciler.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("list node provisions: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("node provisions = %d, want none for stale live workload UID", len(list.Items))
	}
	updated := trainingpolicy.NewObject("TrainingPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train"}, updated); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	reason, _, _ := unstructured.NestedString(updated.Object, "status", trainingpolicy.StatusPolicyPath, "reason")
	if reason != "stale_workload_uid" {
		t.Fatalf("policy reason = %q, want stale_workload_uid", reason)
	}
}

func TestPolicyReconcileReportsReplacementRequiredWithoutSideEffects(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	_ = unstructured.SetNestedField(policy.Object, int64(1), "spec", "policy", "minOnDemand")
	old := trainingpolicy.NewNodeProvision(trainingpolicy.ReadPolicySpec(policy), 0, "Spot")
	old.SetUID(types.UID("old-node-uid"))
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"), old)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile: %v", err)
	}
	replacement := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-00-old-node-uid-replacement"}, replacement); err == nil {
		t.Fatal("policy reconcile created replacement NodeProvision without explicit SpotReplacement opt-in")
	}
	operation := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-00-replace"}, operation); err == nil {
		t.Fatal("policy reconcile created SpotReplacement without explicit opt-in")
	}
	updated := trainingpolicy.NewObject("TrainingPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train"}, updated); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if reason := stringField(updated.Object, "status", trainingpolicy.StatusPolicyPath, "reason"); reason != "replacement_required" {
		t.Fatalf("policy reason = %q, want replacement_required", reason)
	}
	if op := stringField(updated.Object, "status", trainingpolicy.StatusPolicyPath, "replacementOperation"); op != "train-worker-00-old-node-uid-replace" {
		t.Fatalf("replacement operation = %q, want train-worker-00-old-node-uid-replace", op)
	}
	if name := stringField(updated.Object, "status", trainingpolicy.StatusPolicyPath, "replacementNodeProvision"); name != "train-worker-00-old-node-uid-replacement" {
		t.Fatalf("replacement node provision = %q, want train-worker-00-old-node-uid-replacement", name)
	}
}

func TestPolicyReconcileCannotReplaceWithOnlySurvivorBaseline(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	_ = unstructured.SetNestedField(policy.Object, int64(2), "spec", "targetWorkers")
	_ = unstructured.SetNestedField(policy.Object, int64(2), "spec", "policy", "minOnDemand")
	_ = unstructured.SetNestedField(policy.Object, true, "spec", "replacement", "enabled")
	old := trainingpolicy.NewNodeProvision(trainingpolicy.ReadPolicySpec(policy), 1, "Spot")
	old.SetUID(types.UID("old-node-uid"))
	old.Object["status"] = map[string]interface{}{"nodeName": "train-worker-01"}
	runtimeObj := runtimeFixtureWithPods(now, []interface{}{
		map[string]interface{}{"name": "trainer-0", "uid": "survivor-pod-uid", "rank": int64(0), "nodeName": "train-worker-00", "checkpointID": "ckpt-1", "observedAt": now.Format(time.RFC3339)},
		map[string]interface{}{"name": "trainer-1", "uid": "target-pod-uid", "rank": int64(1), "nodeName": "train-worker-01", "checkpointID": "ckpt-1", "observedAt": now.Format(time.RFC3339)},
	})
	risk := riskFixture(now)
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"), old)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("policy reconcile: %v", err)
	}
	op := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-01-old-node-uid-replace"}, op); err == nil {
		t.Fatal("automatic replacement incorrectly selected partial recovery")
	}
	updated := trainingpolicy.NewObject("TrainingPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train"}, updated); err != nil {
		t.Fatal(err)
	}
	if !boolField(updated.Object, "status", "policy", "provisioningBlocked") {
		t.Fatal("missing durable group checkpoint did not block replacement")
	}
}

func TestPolicyReconcileAdoptsCompletedReplacementSuccessor(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	_ = unstructured.SetNestedField(policy.Object, int64(2), "spec", "targetWorkers")
	_ = unstructured.SetNestedField(policy.Object, int64(1), "spec", "policy", "minOnDemand")
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	worker1 := trainingpolicy.NewNodeProvision(trainingpolicy.ReadPolicySpec(policy), 1, "Spot")
	worker1.SetUID(types.UID("worker-01-uid"))
	replacement := trainingpolicy.NewObject("NodeProvision")
	replacement.SetNamespace("default")
	replacement.SetName("train-worker-00-replacement")
	replacement.SetUID(types.UID("replacement-uid"))
	replacement.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy-uid", trainingpolicy.LabelRole: "replacement"})
	replacement.Object["spec"] = map[string]interface{}{"marketType": "OnDemand", "hostname": "train-worker-00-replacement"}
	recovery := trainingpolicy.NewObject("SpotRecovery")
	recovery.SetNamespace("default")
	recovery.SetName("train-worker-00-old-node-uid-replace-cleanup")
	recovery.SetUID(types.UID("recovery-uid"))
	recovery.Object["spec"] = map[string]interface{}{
		"policyRef":                   map[string]interface{}{"name": "train", "uid": "policy-uid", "generation": int64(1)},
		"oldNodeProvisionRef":         map[string]interface{}{"name": "train-worker-00", "uid": "old-node-uid"},
		"replacementNodeProvisionRef": map[string]interface{}{"name": "train-worker-00-replacement", "uid": "replacement-uid"},
		"restoreRequestRef":           map[string]interface{}{"name": "restore", "uid": "restore-uid", "generation": int64(1)},
		"requestUID":                  "restore-uid",
		"operation":                   "train-worker-00-old-node-uid-replace",
		"sourceCluster":               "aws",
		"targetCluster":               "aws",
		"workloadRef":                 map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload-uid"},
		"trainingRuntimeRef":          map[string]interface{}{"name": "train-runtime", "uid": "runtime-uid"},
		"checkpointID":                "ckpt-1",
		"eventID":                     "event-1",
	}
	recovery.Object["status"] = map[string]interface{}{"phase": "Completed"}
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"), worker1, replacement, recovery)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}

	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("policy reconcile %d: %v", i, err)
		}
	}
	list := trainingpolicy.NewList("NodeProvision")
	if err := reconciler.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("list node provisions: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("live NodeProvision count = %d, want 2", len(list.Items))
	}
	old := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-worker-00"}, old); err == nil {
		t.Fatal("old generated slot was recreated after completed replacement cleanup")
	}
}

func TestIsTerminalPhaseUsesSourceClusterStatus(t *testing.T) {
	migration := trainingpolicy.NewObject("FluidCRMigration")
	migration.SetGeneration(3)
	migration.Object["status"] = map[string]interface{}{
		"phase": "Completed",
		"clusters": []interface{}{
			map[string]interface{}{"clusterName": "source", "phase": "Running", "observedGeneration": int64(3)},
			map[string]interface{}{"clusterName": "other", "phase": "Completed", "observedGeneration": int64(3)},
		},
	}
	if isTerminalPhase(migration, "source") {
		t.Fatal("source cluster Running reported terminal because top-level/other status leaked in")
	}
	mustSetNested(t, migration.Object, "Completed", "status", "clusters", "0", "status", "phase")
	if !isTerminalPhase(migration, "source") {
		t.Fatal("source cluster Completed did not report terminal")
	}
}

func TestCheckpointReconcileRepairsOrphanPropagationPolicyForInflight(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:30Z")
	policy := checkpointPolicyFixture(now)
	migration := trainingpolicy.NewFluidCRMigration(trainingpolicy.ReadPolicySpec(policy), trainingpolicy.RuntimeSnapshot{Port: 8298}, now.Add(-30*time.Second), 60)
	migration.SetGeneration(1)
	migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "source", "phase": "Running", "observedGeneration": int64(1)}}}
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, migration, workloadFixture("workload-uid"))

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("reconcile orphan PP: %v", err)
	}
	pp := trainingpolicy.NewObject("PropagationPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: migration.GetName() + "-placement"}, pp); err != nil {
		t.Fatalf("expected repaired PropagationPolicy: %v", err)
	}
	if got := pp.GetLabels()[trainingpolicy.LabelPolicyUID]; got != "policy-uid" {
		t.Fatalf("pp policy uid = %q", got)
	}
}

func TestCheckpointReconcileCreatesTwoRoundsWithFlatRICStatus(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	clock := now
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return clock }, policy, runtimeObj, risk, workloadFixture("workload-uid"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first checkpoint reconcile: %v", err)
	}
	first := getOnlyMigration(t, reconciler.Client)
	markMigrationFlatCompleted(first)
	if err := reconciler.Update(context.Background(), first); err != nil {
		t.Fatalf("update first migration: %v", err)
	}
	clock = now.Add(61 * time.Second)
	refreshRuntimeObservedAt(t, reconciler.Client, clock)
	refreshRiskValidUntil(t, reconciler.Client, clock)
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second checkpoint reconcile: %v", err)
	}
	list := trainingpolicy.NewList("FluidCRMigration")
	if err := reconciler.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("migration count = %d, want 2", len(list.Items))
	}
}

func TestCheckpointReconcileBlocksNewCheckpointWhenLiveWorkloadUIDStale(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("new-workload-uid"))

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("checkpoint reconcile: %v", err)
	}
	list := trainingpolicy.NewList("FluidCRMigration")
	if err := reconciler.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("migrations = %d, want none for stale live workload UID", len(list.Items))
	}
	updated := trainingpolicy.NewObject("TrainingPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train"}, updated); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	reason, _, _ := unstructured.NestedString(updated.Object, "status", trainingpolicy.StatusCheckpointPath, "reason")
	if reason != "stale_workload_uid" {
		t.Fatalf("checkpoint reason = %q, want stale_workload_uid", reason)
	}
}

func TestCheckpointPaperMissingMeasurementsStillCreates(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	_ = unstructured.SetNestedField(policy.Object, true, "spec", "checkpoint", "paperProfile", "enabled")
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeFixture(now), riskFixture(now), workloadFixture("workload-uid"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	getOnlyMigration(t, reconciler.Client)
	if err := reconciler.Get(context.Background(), req.NamespacedName, policy); err != nil {
		t.Fatal(err)
	}
	message, _, _ := unstructured.NestedString(policy.Object, "status", "checkpoint", "message")
	if !strings.Contains(message, "bootstrap interval") {
		t.Fatalf("missing bootstrap provenance: %q", message)
	}
}

func policyReconcilerFixture(t *testing.T, clock func() time.Time, objects ...*unstructured.Unstructured) *PolicyReconciler {
	t.Helper()
	statusObjects := make([]client.Object, 0, len(objects))
	for _, obj := range objects {
		if obj.GetKind() == "TrainingPolicy" {
			statusObjects = append(statusObjects, obj)
		}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(statusObjects...).Build()
	for _, obj := range objects {
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %s/%s fixture: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	// These fixtures exercise allocation after successful discovery and suspended
	// capacity preparation. Discovery/gate rejection is covered independently.
	for _, obj := range objects {
		if obj.GetKind() != "TrainingPolicy" {
			continue
		}
		sts := trainingpolicy.NewObject("StatefulSet")
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: obj.GetNamespace(), Name: stringField(obj.Object, "spec", "workloadRef", "name")}, sts); err != nil {
			continue
		}
		input := trainingpolicy.ReadPolicyInput(obj)
		_ = unstructured.SetNestedField(sts.Object, input.TargetWorkers, "spec", "replicas")
		if err := c.Update(context.Background(), sts); err != nil {
			t.Fatal(err)
		}
		rb := bindingObject()
		rb.SetNamespace(sts.GetNamespace())
		rb.SetName(sts.GetName() + "-statefulset")
		rb.SetUID("binding-uid")
		rb.Object["spec"] = map[string]interface{}{
			"resource":   map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "namespace": sts.GetNamespace(), "name": sts.GetName(), "uid": string(sts.GetUID())},
			"clusters":   []interface{}{map[string]interface{}{"name": input.Capacity.AWSCluster, "replicas": input.TargetWorkers}},
			"suspension": map[string]interface{}{"dispatching": input.SourceCluster != input.Capacity.AWSCluster},
		}
		if err := c.Create(context.Background(), rb); err != nil {
			t.Fatal(err)
		}
	}
	return &PolicyReconciler{Client: c, Clock: clock}
}

func checkpointReconcilerFixture(t *testing.T, clock func() time.Time, objects ...*unstructured.Unstructured) *CheckpointReconciler {
	t.Helper()
	statusObjects := make([]client.Object, 0, len(objects))
	for _, obj := range objects {
		if obj.GetKind() == "TrainingPolicy" {
			statusObjects = append(statusObjects, obj)
		}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(statusObjects...).Build()
	for _, obj := range objects {
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %s/%s fixture: %v", obj.GetKind(), obj.GetName(), err)
		}
		created := trainingpolicy.NewObject(obj.GetKind())
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}, created); err != nil {
			t.Fatalf("get %s/%s fixture after create: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	return &CheckpointReconciler{Client: c, Clock: clock}
}

func mustSetNested(t *testing.T, obj map[string]interface{}, value interface{}, fields ...string) {
	t.Helper()
	clusters := obj["status"].(map[string]interface{})["clusters"].([]interface{})
	clusters[0].(map[string]interface{})["phase"] = value
}

var _ runtime.Object = (&unstructured.Unstructured{}).DeepCopyObject()

func checkpointPolicyFixture(now time.Time) *unstructured.Unstructured {
	policy := trainingpolicy.NewObject("TrainingPolicy")
	policy.SetNamespace("default")
	policy.SetName("train")
	policy.SetUID(types.UID("policy-uid"))
	policy.SetGeneration(1)
	policy.Object["spec"] = map[string]interface{}{
		"workloadRef":   map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload-uid"},
		"sourceCluster": "source",
		"targetWorkers": int64(1),
		"checkpoint":    map[string]interface{}{"candidateIntervalSeconds": []interface{}{int64(60)}, "riskBands": []interface{}{map[string]interface{}{"maxLambdaPerHour": float64(1), "intervalSeconds": int64(60)}}},
	}
	policy.Object["status"] = map[string]interface{}{"discovery": map[string]interface{}{
		"ready": true, "observedGeneration": int64(1), "workloadUID": "workload-uid",
	}}
	return policy
}

func runtimeFixture(now time.Time) *unstructured.Unstructured {
	return runtimeFixtureWithPods(now, []interface{}{map[string]interface{}{"name": "rank-0", "uid": "pod-uid", "rank": int64(0), "nodeName": "train-worker-00", "checkpointID": "ckpt-1", "observedAt": now.Format(time.RFC3339)}})
}

func runtimeFixtureWithPods(now time.Time, pods []interface{}) *unstructured.Unstructured {
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	runtimeObj.SetNamespace("default")
	runtimeObj.SetName("train-runtime")
	runtimeObj.SetGeneration(1)
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}, "sourceCluster": "source", "expectedWorldSize": int64(len(pods)), "port": int64(8298)}
	runtimeObj.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "source", "status": map[string]interface{}{"phase": "Running", "workloadUID": "workload-uid", "memberWorkloadUID": "workload-uid", "observedGeneration": int64(1), "observedAt": now.Format(time.RFC3339), "readyRanks": int64(len(pods)), "worldSize": int64(len(pods)), "pods": pods}}}}
	return runtimeObj
}

func riskFixture(now time.Time) *unstructured.Unstructured {
	risk := trainingpolicy.NewObject("SpotRiskProfile")
	risk.SetNamespace("default")
	risk.SetName("train-risk")
	risk.SetGeneration(1)
	risk.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "ready": true, "lambdaPerHour": float64(0.01), "validUntil": now.Add(5 * time.Minute).Format(time.RFC3339)}
	return risk
}

func workloadFixture(uid types.UID) *unstructured.Unstructured {
	workload := trainingpolicy.NewObject("StatefulSet")
	workload.SetNamespace("default")
	workload.SetName("trainer")
	workload.SetUID(uid)
	return workload
}

func getOnlyMigration(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	list := trainingpolicy.NewList("FluidCRMigration")
	if err := c.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("migration count = %d, want 1", len(list.Items))
	}
	return &list.Items[0]
}

func markMigrationFlatCompleted(migration *unstructured.Unstructured) {
	migration.SetGeneration(1)
	migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "source", "phase": "Completed", "observedGeneration": int64(1)}}}
}

func refreshRuntimeObservedAt(t *testing.T, c client.Client, at time.Time) {
	t.Helper()
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-runtime"}, runtimeObj); err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	clusters, _, _ := unstructured.NestedSlice(runtimeObj.Object, "status", "clusters")
	status := clusters[0].(map[string]interface{})["status"].(map[string]interface{})
	status["observedAt"] = at.Format(time.RFC3339)
	pods := status["pods"].([]interface{})
	pods[0].(map[string]interface{})["observedAt"] = at.Format(time.RFC3339)
	_ = unstructured.SetNestedSlice(runtimeObj.Object, clusters, "status", "clusters")
	if err := c.Update(context.Background(), runtimeObj); err != nil {
		t.Fatalf("update runtime: %v", err)
	}
}

func refreshRiskValidUntil(t *testing.T, c client.Client, at time.Time) {
	t.Helper()
	risk := trainingpolicy.NewObject("SpotRiskProfile")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train-risk"}, risk); err != nil {
		t.Fatalf("get risk: %v", err)
	}
	_ = unstructured.SetNestedField(risk.Object, at.Add(5*time.Minute).Format(time.RFC3339), "status", "validUntil")
	if err := c.Update(context.Background(), risk); err != nil {
		t.Fatalf("update risk: %v", err)
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	register := func(gvk schema.GroupVersionKind) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		list := &unstructured.UnstructuredList{}
		listGVK := gvk
		listGVK.Kind += "List"
		list.SetGroupVersionKind(listGVK)
		scheme.AddKnownTypes(gvk.GroupVersion(), obj, list)
	}
	register(trainingpolicy.TrainingPolicyGVK)
	register(trainingpolicy.StatefulSetGVK)
	register(trainingpolicy.TrainingRuntimeGVK)
	register(trainingpolicy.SpotRiskProfileGVK)
	register(trainingpolicy.SpotRecoveryGVK)
	register(spotReplacementGVK)
	register(trainingpolicy.NodeProvisionGVK)
	register(trainingpolicy.FluidMigrationGVK)
	register(trainingpolicy.PropagationPolicyGVK)
	return scheme
}
