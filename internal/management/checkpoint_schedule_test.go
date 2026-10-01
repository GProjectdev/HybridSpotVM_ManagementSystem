package management

import (
	"context"
	"strings"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCheckpointScheduleStableNameUpdatesScheduleOnly(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	input := p.ReadPolicyInput(policy)
	runtimeSnapshot := p.RuntimeSnapshot{Port: 8298}
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now })

	first, err := reconciler.ensureCheckpointSchedule(context.Background(), input, runtimeSnapshot, 60)
	if err != nil {
		t.Fatalf("ensure first schedule: %v", err)
	}
	if first.GetName() != "train-periodic" || first.GetAnnotations() != nil {
		t.Fatalf("first schedule identity = %s annotations=%v, want stable name without round annotations", first.GetName(), first.GetAnnotations())
	}
	if got := intField(first.Object, "spec", "schedule", "intervalSeconds"); got != 60 {
		t.Fatalf("first interval = %d, want 60", got)
	}

	first.Object["spec"].(map[string]interface{})["schemaDefault"] = "preserve-me"
	if err := reconciler.Update(context.Background(), first); err != nil {
		t.Fatalf("add schema default to schedule: %v", err)
	}
	second, err := reconciler.ensureCheckpointSchedule(context.Background(), input, runtimeSnapshot, 120)
	if err != nil {
		t.Fatalf("ensure updated schedule: %v", err)
	}
	if second.GetName() != first.GetName() || second.GetUID() != first.GetUID() {
		t.Fatalf("schedule was replaced: first %s/%s second %s/%s", first.GetName(), first.GetUID(), second.GetName(), second.GetUID())
	}
	if got := intField(second.Object, "spec", "schedule", "intervalSeconds"); got != 120 {
		t.Fatalf("updated interval = %d, want 120", got)
	}
	if !boolField(second.Object, "spec", "schedule", "enabled") {
		t.Fatal("updated schedule did not stay enabled")
	}
	if got := stringField(second.Object, "spec", "schemaDefault"); got != "preserve-me" {
		t.Fatalf("immutable/defaulted spec field = %q, want preserved", got)
	}
}

func TestCheckpointScheduleRejectsImmutableRuntimeConfigDrift(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	input := p.ReadPolicyInput(policy)
	reconciler := checkpointReconcilerFixture(t, func() time.Time { return now })
	originalRuntime := p.RuntimeSnapshot{Port: 8298, Container: "trainer"}

	if _, err := reconciler.ensureCheckpointSchedule(context.Background(), input, originalRuntime, 60); err != nil {
		t.Fatalf("ensure original schedule: %v", err)
	}
	_, err := reconciler.ensureCheckpointSchedule(context.Background(), input, p.RuntimeSnapshot{Port: 9000, Container: "sidecar"}, 60)
	if err == nil || (!strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "drift")) {
		t.Fatalf("drift error = %v, want immutable config drift rejection", err)
	}
}
func TestCheckpointSchedulePauseRequiresDisabledFreshPausedSource(t *testing.T) {
	input := schedulePolicyInput()
	for _, tc := range []struct {
		name               string
		enabled            bool
		phase              string
		observedGeneration int64
		wantReady          bool
	}{
		{name: "enabled-is-first-disabled", enabled: true, phase: "Paused", observedGeneration: 3, wantReady: false},
		{name: "stale-paused-source", phase: "Paused", observedGeneration: 2, wantReady: false},
		{name: "fresh-running-source", phase: "Running", observedGeneration: 3, wantReady: false},
		{name: "fresh-disabled-paused-source", phase: "Paused", observedGeneration: 3, wantReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schedule := checkpointScheduleFixture(input, tc.enabled)
			schedule.SetGeneration(3)
			schedule.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{
				"clusterName": input.SourceCluster, "phase": tc.phase, "observedGeneration": tc.observedGeneration,
			}}}
			reconciler := checkpointReconcilerFixture(t, func() time.Time { return time.Now() }, schedule)

			ready, err := reconciler.pauseCheckpointSchedules(context.Background(), input)
			if err != nil {
				t.Fatalf("pause schedules: %v", err)
			}
			if ready != tc.wantReady {
				t.Fatalf("ready = %v, want %v", ready, tc.wantReady)
			}
			got := p.NewObject("FluidCRMigration")
			if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(schedule), got); err != nil {
				t.Fatalf("get schedule: %v", err)
			}
			if boolField(got.Object, "spec", "schedule", "enabled") {
				t.Fatal("schedule remained enabled after pause request")
			}
		})
	}
}

func TestCheckpointScheduleMirrorsImmutableCompleteFullEvidence(t *testing.T) {
	parent, input, checkpoint := scheduledEvidenceFixtures(t)
	reconciler := checkpointScheduleReconcilerFixture(t, func() time.Time { return time.Now() }, parent)

	if err := reconciler.syncScheduledEvidence(context.Background(), input); err != nil {
		t.Fatalf("sync scheduled evidence: %v", err)
	}
	retained := p.NewObject("FluidCRMigration")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: input.Namespace, Name: checkpoint.GetName()}, retained); err != nil {
		t.Fatalf("get retained evidence: %v", err)
	}
	if got := retained.GetLabels()[p.LabelRole]; got != checkpointEvidenceRole {
		t.Fatalf("retained role = %q, want %q", got, checkpointEvidenceRole)
	}
	if retained.GetAnnotations()["training.dcnlab.com/checkpoint-id"] != "member-round-hash" {
		t.Fatalf("retained checkpoint-id annotation = %q, want member child checkpointID", retained.GetAnnotations()["training.dcnlab.com/checkpoint-id"])
	}
	if retained.GetAnnotations()["training.dcnlab.com/schedule-uid"] != string(parent.GetUID()) ||
		retained.GetAnnotations()["training.dcnlab.com/source-checkpoint-uid"] != string(checkpoint.GetUID()) {
		t.Fatalf("retained annotations = %#v", retained.GetAnnotations())
	}
	if _, found, _ := unstructured.NestedMap(retained.Object, "spec", "schedule"); found {
		t.Fatal("retained evidence copied schedule spec")
	}
	clusters, found, _ := unstructured.NestedSlice(retained.Object, "status", "clusters")
	if !found || len(clusters) != 1 || stringField(clusters[0].(map[string]interface{}), "clusterName") != input.SourceCluster {
		t.Fatalf("retained status clusters = %#v, want source-cluster-only evidence", retained.Object["status"])
	}
	validationCopy := retained.DeepCopy()
	validationCopy.SetUID(types.UID("retained-uid"))
	validationCopy.SetGeneration(1)
	if _, err := readGroupCheckpoint(validationCopy, input); err != nil {
		t.Fatalf("retained evidence is not a complete full checkpoint: %v", err)
	}

	groupReport(retained)["completionTime"] = "2026-09-28T02:00:00Z"
	if err := reconciler.Status().Update(context.Background(), retained); err != nil {
		t.Fatalf("mutate retained status: %v", err)
	}
	if err := reconciler.syncScheduledEvidence(context.Background(), input); err == nil || !strings.Contains(err.Error(), "retained checkpoint evidence changed") {
		t.Fatalf("sync after retained mutation err = %v, want immutable evidence rejection", err)
	}
}

func TestCheckpointScheduleRejectsCorruptOrPartialMirroredEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "partial", mutate: func(cp *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(cp.Object, map[string]interface{}{"targetRanks": []interface{}{int64(1)}}, "spec", "partialCheckpoint")
		}},
		{name: "nested-schedule", mutate: func(cp *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(cp.Object, map[string]interface{}{"enabled": true}, "spec", "schedule")
		}},
		{name: "corrupt-result", mutate: func(cp *unstructured.Unstructured) {
			delete(groupFile(cp), "exportedAt")
		}},
		{name: "mixed-archive-round", mutate: func(cp *unstructured.Unstructured) {
			groupFile(cp)["checkpointID"] = "other-round"
		}},
		{name: "mixed-pod-round", mutate: func(cp *unstructured.Unstructured) {
			groupReport(cp)["pods"].([]interface{})[0].(map[string]interface{})["checkpointID"] = "other-round"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, input, checkpoint := scheduledEvidenceFixtures(t)
			tc.mutate(checkpoint)
			setLastSuccessfulFullCheckpoint(t, parent, checkpoint)
			reconciler := checkpointScheduleReconcilerFixture(t, func() time.Time { return time.Now() }, parent)

			if err := reconciler.syncScheduledEvidence(context.Background(), input); err == nil {
				t.Fatal("corrupt scheduled evidence was mirrored")
			}
			retained := p.NewObject("FluidCRMigration")
			if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: input.Namespace, Name: checkpoint.GetName()}, retained); err == nil {
				t.Fatal("retained evidence object was created for rejected source")
			}
		})
	}
}

func TestCheckpointScheduleLatestFailedParentKeepsRetainedGoodEvidence(t *testing.T) {
	parent, input, checkpoint := scheduledEvidenceFixtures(t)
	reconciler := checkpointScheduleReconcilerFixture(t, func() time.Time { return time.Now() }, parent)
	if err := reconciler.syncScheduledEvidence(context.Background(), input); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	failedParent := checkpointScheduleFixture(input, false)
	failedParent.SetName("train-periodic-failed")
	failedParent.SetUID(types.UID("failed-schedule"))
	failedParent.SetGeneration(9)
	failedParent.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{
		"clusterName": input.SourceCluster, "phase": "Failed", "observedGeneration": int64(9),
	}}}
	if err := reconciler.Create(context.Background(), failedParent); err != nil {
		t.Fatalf("create failed parent: %v", err)
	}
	if err := reconciler.syncScheduledEvidence(context.Background(), input); err != nil {
		t.Fatalf("sync after failed parent: %v", err)
	}

	retained := p.NewObject("FluidCRMigration")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: input.Namespace, Name: checkpoint.GetName()}, retained); err != nil {
		t.Fatalf("retained checkpoint was lost after failed parent: %v", err)
	}
	if stringField(groupReport(retained), "completionTime") != "2026-09-28T00:00:00Z" {
		t.Fatalf("retained checkpoint evidence changed after failed parent: %#v", retained.Object["status"])
	}
}

func TestCheckpointReconcileDoesNotPropagateRetainedEvidence(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	_ = unstructured.SetNestedField(policy.Object, int64(2), "spec", "targetWorkers")
	runtimeObj := runtimeFixtureWithPods(now, []interface{}{
		map[string]interface{}{"name": "trainer-0", "uid": "trainer-0-uid", "rank": int64(0), "nodeName": "train-worker-00", "checkpointID": "ckpt-1", "observedAt": now.Format(time.RFC3339)},
		map[string]interface{}{"name": "trainer-1", "uid": "trainer-1-uid", "rank": int64(1), "nodeName": "train-worker-01", "checkpointID": "ckpt-1", "observedAt": now.Format(time.RFC3339)},
	})
	risk := riskFixture(now)
	parent, _, checkpoint := scheduledEvidenceFixtures(t)
	input := p.ReadPolicyInput(policy)
	parent.SetNamespace(policy.GetNamespace())
	parent.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID), p.LabelPolicy: policy.GetName(), p.LabelManagedBy: "hybridspotvm-system", p.LabelRole: checkpointScheduleRole})
	checkpoint.SetNamespace(policy.GetNamespace())
	checkpoint.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID)})
	normalizeCheckpointForInput(checkpoint, input)
	_ = unstructured.SetNestedField(parent.Object, string(input.WorkloadRef.UID), "spec", "workloadRef", "uid")
	_ = unstructured.SetNestedField(parent.Object, input.WorkloadRef.Name, "spec", "workloadRef", "name")
	_ = unstructured.SetNestedField(parent.Object, int64(8298), "spec", "ctrlPort")
	_ = unstructured.SetNestedField(checkpoint.Object, string(input.WorkloadRef.UID), "spec", "workloadRef", "uid")
	_ = unstructured.SetNestedField(checkpoint.Object, input.WorkloadRef.Name, "spec", "workloadRef", "name")
	parent.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})["clusterName"] = input.SourceCluster
	groupReport(checkpoint)["clusterName"] = input.SourceCluster
	setLastSuccessfulFullCheckpoint(t, parent, checkpoint)
	reconciler := checkpointScheduleReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, workloadFixture("workload-uid"), parent)
	if err := reconciler.syncScheduledEvidence(context.Background(), input); err != nil {
		t.Fatalf("seed retained evidence: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: policy.GetNamespace(), Name: policy.GetName()}}); err != nil {
		t.Fatalf("checkpoint reconcile: %v", err)
	}
	pp := p.NewObject("PropagationPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: policy.GetNamespace(), Name: checkpoint.GetName() + "-placement"}, pp); err == nil {
		t.Fatal("reconcile propagated retained evidence")
	}
}

func scheduledEvidenceFixtures(t *testing.T) (*unstructured.Unstructured, p.PolicyInput, *unstructured.Unstructured) {
	t.Helper()
	checkpoint, input := groupCheckpointFixture()
	input.PolicyName = "train"
	parent := checkpointScheduleFixture(input, false)
	parent.SetUID(types.UID("schedule-uid"))
	parent.SetGeneration(3)
	parent.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{
		"clusterName": input.SourceCluster, "phase": "Paused", "observedGeneration": int64(3),
	}}}
	setLastSuccessfulFullCheckpoint(t, parent, checkpoint)
	return parent, input, checkpoint
}

func checkpointScheduleFixture(input p.PolicyInput, enabled bool) *unstructured.Unstructured {
	obj := p.NewObject("FluidCRMigration")
	obj.SetNamespace(input.Namespace)
	obj.SetName(input.PolicyName + "-periodic")
	obj.SetUID(types.UID(input.PolicyName + "-periodic-uid"))
	obj.SetGeneration(1)
	obj.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID), p.LabelPolicy: input.PolicyName, p.LabelManagedBy: "hybridspotvm-system", p.LabelRole: checkpointScheduleRole})
	obj.Object["spec"] = map[string]interface{}{
		"workloadRef": map[string]interface{}{"apiVersion": input.WorkloadRef.APIVersion, "kind": input.WorkloadRef.Kind, "name": input.WorkloadRef.Name, "uid": string(input.WorkloadRef.UID)},
		"schedule":    map[string]interface{}{"enabled": enabled, "intervalSeconds": int64(60)},
	}
	return obj
}

func schedulePolicyInput() p.PolicyInput {
	return p.PolicyInput{
		Namespace: "demo", PolicyName: "train", PolicyUID: types.UID("policy"), SourceCluster: "aws", TargetWorkers: 2,
		WorkloadRef: p.WorkloadRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "trainer", UID: types.UID("workload")},
		Checkpoint:  p.CheckpointPolicy{Resume: true},
	}
}

func setLastSuccessfulFullCheckpoint(t *testing.T, parent, checkpoint *unstructured.Unstructured) {
	t.Helper()
	spec, found, err := unstructured.NestedMap(checkpoint.Object, "spec")
	if err != nil || !found {
		t.Fatalf("checkpoint spec: found=%v err=%v", found, err)
	}
	status := copyStringMap(groupReport(checkpoint))
	ref := map[string]interface{}{
		"name":         checkpoint.GetName(),
		"uid":          string(checkpoint.GetUID()),
		"checkpointID": "member-round-hash",
		"result":       map[string]interface{}{"spec": spec, "status": status},
	}
	clusters, found, _ := unstructured.NestedSlice(parent.Object, "status", "clusters")
	if !found || len(clusters) != 1 {
		t.Fatalf("parent source status missing: %#v", parent.Object["status"])
	}
	clusters[0].(map[string]interface{})["lastSuccessfulFullCheckpoint"] = ref
	_ = unstructured.SetNestedSlice(parent.Object, clusters, "status", "clusters")
}

func checkpointScheduleReconcilerFixture(t *testing.T, clock func() time.Time, objects ...*unstructured.Unstructured) *CheckpointReconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(p.NewObject("TrainingPolicy"), p.NewObject("FluidCRMigration")).Build()
	for _, obj := range objects {
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %s/%s fixture: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	return &CheckpointReconciler{Client: c, Clock: clock}
}

func normalizeCheckpointForInput(checkpoint *unstructured.Unstructured, input p.PolicyInput) {
	checkpoint.SetNamespace(input.Namespace)
	checkpoint.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID)})
	_ = unstructured.SetNestedField(checkpoint.Object, string(input.WorkloadRef.UID), "spec", "workloadRef", "uid")
	_ = unstructured.SetNestedField(checkpoint.Object, input.WorkloadRef.Name, "spec", "workloadRef", "name")
	groupReport(checkpoint)["clusterName"] = input.SourceCluster
	for _, pod := range groupReport(checkpoint)["pods"].([]interface{}) {
		for _, file := range pod.(map[string]interface{})["checkpointFiles"].([]interface{}) {
			archive := file.(map[string]interface{})
			archive["durableRef"] = "file-store:" + input.Namespace + "/sha256/" + stringField(archive, "sha256")
		}
	}
}
func copyStringMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
