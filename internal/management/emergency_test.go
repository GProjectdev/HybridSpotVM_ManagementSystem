package management

import (
	"context"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEmergencyCheckpointIgnoresExpiredRiskAndPersistsEventAnnotations(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	runtimeObj := runtimeFixture(now)
	risk := riskFixture(now)
	_ = unstructured.SetNestedField(risk.Object, now.Add(-time.Minute).Format(time.RFC3339), "status", "validUntil")
	np := emergencyNodeProvision("train-worker-00", "np-uid", "policy-uid", "aws", "i-123", "event-1")
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"), np)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("emergency reconcile: %v", err)
	}
	migration := getOnlyMigration(t, reconciler.Client)
	annotations := migration.GetAnnotations()
	if annotations[annotationEmergencyEventID] != "event-1" || annotations[annotationEmergencyInstanceID] != "i-123" {
		t.Fatalf("emergency annotations = %#v", annotations)
	}
	if annotations["training.dcnlab.com/checkpoint-id"] != migration.GetName() {
		t.Fatalf("checkpoint-id annotation = %q, want migration name %q", annotations["training.dcnlab.com/checkpoint-id"], migration.GetName())
	}
}

func TestEmergencyCheckpointDedupsSameEventAfterRestart(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	runtimeObj := runtimeFixture(now)
	np := emergencyNodeProvision("train-worker-00", "np-uid", "policy-uid", "aws", "i-123", "event-1")
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, workloadFixture("workload-uid"), np)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first emergency reconcile: %v", err)
	}
	first := getOnlyMigration(t, reconciler.Client)
	markMigrationFlatCompleted(first)
	if err := reconciler.Update(context.Background(), first); err != nil {
		t.Fatalf("complete first migration: %v", err)
	}
	now = now.Add(10 * time.Minute)
	refreshRuntimeObservedAt(t, reconciler.Client, now)
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second emergency reconcile: %v", err)
	}
	assertMigrationCount(t, reconciler.Client, 1)
}

func TestEmergencyCheckpointDedupsDuplicateNodesForSameEvent(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	runtimeObj := runtimeFixture(now)
	np1 := emergencyNodeProvision("train-worker-00", "np-uid-0", "policy-uid", "aws", "i-123", "event-1")
	np2 := emergencyNodeProvision("train-worker-01", "np-uid-1", "policy-uid", "aws", "i-456", "event-1")
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, workloadFixture("workload-uid"), np1, np2)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("emergency reconcile: %v", err)
	}
	assertMigrationCount(t, reconciler.Client, 1)
}

func TestEmergencyCheckpointSkipsHandledEventAndCreatesForNextEvent(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	runtimeObj := runtimeFixture(now)
	np1 := emergencyNodeProvision("train-worker-00", "np-uid-0", "policy-uid", "aws", "i-123", "event-1")
	np2 := emergencyNodeProvision("train-worker-01", "np-uid-1", "policy-uid", "aws", "i-456", "event-2")
	input := trainingpolicy.ReadPolicySpec(policy)
	handled := newEmergencyCheckpoint(input, trainingpolicy.RuntimeSnapshot{Port: 8298}, emergencySpotEvent{EventID: "event-1", InstanceID: "i-123", NodeName: "train-worker-00", NodeUID: "np-uid-0"}, now.Add(-time.Minute), 60)
	markMigrationFlatCompleted(handled)
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, workloadFixture("workload-uid"), np1, np2, handled)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("emergency reconcile: %v", err)
	}
	assertMigrationCount(t, reconciler.Client, 2)
	migrations := trainingpolicy.NewList("FluidCRMigration")
	if err := reconciler.List(context.Background(), migrations, client.InNamespace("default")); err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	foundSecond := false
	for i := range migrations.Items {
		annotations := migrations.Items[i].GetAnnotations()
		if annotations[annotationEmergencyEventID] == "event-2" && annotations[annotationEmergencyInstanceID] == "i-456" {
			foundSecond = true
		}
	}
	if !foundSecond {
		t.Fatalf("second emergency event was not checkpointed: %#v", migrations.Items)
	}
}

func TestEmergencyCheckpointRequiresRuntimeEvidence(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	np := emergencyNodeProvision("train-worker-00", "np-uid", "policy-uid", "aws", "i-123", "event-1")
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, workloadFixture("workload-uid"), np)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("emergency reconcile: %v", err)
	}
	assertMigrationCount(t, reconciler.Client, 0)
	updated := trainingpolicy.NewObject("TrainingPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "train"}, updated); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	reason, _, _ := unstructured.NestedString(updated.Object, "status", trainingpolicy.StatusCheckpointPath, "reason")
	if reason != "runtime_not_ready" {
		t.Fatalf("checkpoint reason = %q, want runtime_not_ready", reason)
	}
}

func TestEmergencyCheckpointRepairsOrphanPropagationPolicyBeforeTelemetry(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:30Z")
	policy := checkpointPolicyFixture(now)
	input := trainingpolicy.ReadPolicySpec(policy)
	migration := trainingpolicy.NewFluidCRMigration(input, trainingpolicy.RuntimeSnapshot{Port: 8298}, now.Add(-30*time.Second), 60)
	migration.SetName("train-emergency-orphan")
	migration.SetAnnotations(map[string]string{
		"training.dcnlab.com/checkpoint-id": migration.GetName(),
		annotationEmergencyEventID:          "event-1",
	})
	migration.SetGeneration(1)
	migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "source", "phase": "Running", "observedGeneration": int64(1)}}}
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, migration, workloadFixture("workload-uid"))

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "train"}}); err != nil {
		t.Fatalf("orphan emergency reconcile: %v", err)
	}
	pp := trainingpolicy.NewObject("PropagationPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: migration.GetName() + "-placement"}, pp); err != nil {
		t.Fatalf("expected repaired emergency PP: %v", err)
	}
}

func emergencyNodeProvision(name, uid, policyUID, observedCluster, instanceID, eventID string) *unstructured.Unstructured {
	np := trainingpolicy.NewObject("NodeProvision")
	np.SetNamespace("default")
	np.SetName(name)
	np.SetUID(types.UID(uid))
	np.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: policyUID})
	np.Object["status"] = map[string]interface{}{
		"observedCluster": observedCluster,
		"instanceId":      instanceID,
		"spot": map[string]interface{}{
			"atRisk":     true,
			"eventID":    eventID,
			"instanceID": instanceID,
		},
	}
	return np
}

func assertMigrationCount(t *testing.T, c client.Client, want int) {
	t.Helper()
	list := trainingpolicy.NewList("FluidCRMigration")
	if err := c.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(list.Items) != want {
		t.Fatalf("migration count = %d, want %d", len(list.Items), want)
	}
}
