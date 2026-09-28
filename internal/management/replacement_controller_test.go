package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReplacementReconcileCreatesOnDemandCapacityOnlyForExplicitOperation(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	policy := replacementPolicyFixture()
	oldNP := replacementOldNodeProvisionFixture()
	reconciler := replacementReconcilerFixture(t, now, op, policy, oldNP)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	replacement := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "new"}, replacement); err != nil {
		t.Fatalf("replacement NodeProvision: %v", err)
	}
	if market := stringField(replacement.Object, "spec", "marketType"); market != "OnDemand" {
		t.Fatalf("marketType = %q, want OnDemand", market)
	}
	if role := replacement.GetLabels()[trainingpolicy.LabelRole]; role != "replacement" {
		t.Fatalf("role = %q, want replacement", role)
	}
	pp := trainingpolicy.NewObject("PropagationPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "new-placement"}, pp); err != nil {
		t.Fatalf("replacement placement: %v", err)
	}
}

func TestReplacementReconcileCreatesSpotCapacityForOnDemandSource(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	op.Object["spec"].(map[string]interface{})["desiredMarketType"] = "Spot"
	oldNP := replacementOldNodeProvisionFixture()
	oldNP.Object["spec"].(map[string]interface{})["marketType"] = "OnDemand"
	delete(oldNP.Object["status"].(map[string]interface{}), "spot")
	reconciler := replacementReconcilerFixture(t, now, op, replacementPolicyFixture(), oldNP)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	replacement := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "new"}, replacement); err != nil {
		t.Fatalf("replacement NodeProvision: %v", err)
	}
	if market := stringField(replacement.Object, "spec", "marketType"); market != "Spot" {
		t.Fatalf("marketType = %q, want Spot", market)
	}
}

func TestPartialReplacementRejectsInfrastructureFencedSource(t *testing.T) {
	for _, field := range []string{"spec", "status"} {
		t.Run(field, func(t *testing.T) {
			now := mustParseTime(t, "2026-09-26T00:00:00Z")
			op := replacementOperationFixture()
			old := replacementOldNodeProvisionFixture()
			_ = unstructured.SetNestedMap(old.Object, map[string]interface{}{"operationUID": "another-operation", "instanceID": "i-old"}, field, "fence")
			r := replacementReconcilerFixture(t, now, op, replacementPolicyFixture(), old)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), op); err != nil {
				t.Fatal(err)
			}
			if stringField(op.Object, "status", "phase") != "Rejected" {
				t.Fatal("partial recovery must not continue after infrastructure fencing")
			}
			nodes := trainingpolicy.NewList("NodeProvision")
			if err := r.List(context.Background(), nodes); err != nil || len(nodes.Items) != 1 {
				t.Fatalf("unexpected replacement capacity: %v, nodes=%d", err, len(nodes.Items))
			}
		})
	}
}

func TestReplacementEmergencyCreatesPartialCheckpointBeforeReplacementReady(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	op.SetName(replacementOperationName("old", "old-uid"))
	op.Object["spec"].(map[string]interface{})["operation"] = op.GetName()
	op.SetAnnotations(map[string]string{"training.dcnlab.com/emergency-event-id": "event-1"})
	oldNP := replacementOldNodeProvisionFixture()
	setEmergencyReplacementSpotEvidence(oldNP, "event-1")
	reconciler := replacementReconcilerFixture(t, now, op, replacementPolicyFixture(), oldNP)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: op.GetName()}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	migration := trainingpolicy.NewObject("FluidCRMigration")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: op.GetName() + "-partial-checkpoint"}, migration); err != nil {
		t.Fatalf("emergency partial checkpoint: %v", err)
	}
	replacement := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "new"}, replacement); err != nil {
		t.Fatalf("replacement NodeProvision: %v", err)
	}
	updated := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: op.GetName()}, updated); err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase != replacementPhaseAwaitingReplacementReady {
		t.Fatalf("phase = %q, want AwaitingReplacementReady", phase)
	}
}

func TestReplacementEmergencyRejectsAnnotationEventSpoof(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	op.SetName(replacementOperationName("old", "old-uid"))
	op.Object["spec"].(map[string]interface{})["operation"] = op.GetName()
	op.SetAnnotations(map[string]string{"training.dcnlab.com/emergency-event-id": "spoof"})
	oldNP := replacementOldNodeProvisionFixture()
	setEmergencyReplacementSpotEvidence(oldNP, "event-1")
	reconciler := replacementReconcilerFixture(t, now, op, replacementPolicyFixture(), oldNP)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: op.GetName()}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	updated := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: op.GetName()}, updated); err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase != "Rejected" {
		t.Fatalf("phase = %q, want Rejected", phase)
	}
	if msg := stringField(updated.Object, "status", "message"); !strings.Contains(msg, "matching annotation") {
		t.Fatalf("message = %q, want annotation mismatch rejection", msg)
	}
	migration := trainingpolicy.NewObject("FluidCRMigration")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: op.GetName() + "-partial-checkpoint"}, migration); err == nil {
		t.Fatal("partial checkpoint created for spoofed emergency annotation")
	}
}

func TestReplacementWaitsForExistingFullCheckpointBeforePartial(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	inflight := trainingpolicy.NewObject("FluidCRMigration")
	inflight.SetNamespace("default")
	inflight.SetName("full-running")
	inflight.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy-uid", trainingpolicy.LabelRole: "checkpoint"})
	inflight.SetGeneration(1)
	inflight.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "phase": "Running", "observedGeneration": int64(1)}}}
	reconciler := replacementReconcilerFixture(t, now, op, replacementPolicyFixture(), replacementOldNodeProvisionFixture(), replacementReadyNodeProvisionFixture(), inflight)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	migration := trainingpolicy.NewObject("FluidCRMigration")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace-partial-checkpoint"}, migration); err == nil {
		t.Fatal("partial checkpoint was created while full checkpoint was still running")
	}
}

func TestReplacementReconcileRepairsPlacementForExistingReplacementNode(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	reconciler := replacementReconcilerFixture(t, now, replacementOperationFixture(), replacementPolicyFixture(), replacementOldNodeProvisionFixture(), replacementReadyNodeProvisionFixture())

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	pp := trainingpolicy.NewObject("PropagationPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "new-placement"}, pp); err != nil {
		t.Fatalf("expected repaired replacement placement: %v", err)
	}
}

func TestReplacementReconcileRepairsPlacementForExistingPartialCheckpoint(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	migration := replacementPartialCheckpointFixture(false)
	reconciler := replacementReconcilerFixture(t, now, replacementOperationFixture(), replacementPolicyFixture(), replacementOldNodeProvisionFixture(), replacementReadyNodeProvisionFixture(), migration)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	pp := trainingpolicy.NewObject("PropagationPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace-partial-checkpoint-placement"}, pp); err != nil {
		t.Fatalf("expected repaired checkpoint placement: %v", err)
	}
}

func TestReplacementReconcileRepairsPlacementForExistingRestoreRequest(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	restore := newRestoreRequest()
	restore.SetNamespace("default")
	restore.SetName("old-replace-restore")
	restore.SetUID(types.UID("restore-uid"))
	restore.SetGeneration(1)
	restore.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy-uid", trainingpolicy.LabelRole: "replacement-restore"})
	reconciler := replacementReconcilerFixture(t, now, replacementOperationFixture(), replacementPolicyFixture(), replacementRuntimeFixture(), replacementOldNodeProvisionFixture(), replacementReadyNodeProvisionFixture(), replacementPartialCheckpointFixture(true), restore)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	pp := trainingpolicy.NewObject("PropagationPolicy")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace-restore-placement"}, pp); err != nil {
		t.Fatalf("expected repaired restore placement: %v", err)
	}
}

func TestReplacementPhaseSequenceProducesRestoreAndCompletesAfterOldNodeDeletion(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	_ = unstructured.SetNestedSlice(op.Object, []interface{}{map[string]interface{}{"rank": int64(0), "podName": "trainer-0", "podUID": "survivor-pod-uid", "nodeName": "survivor-node"}}, "spec", "partialRestore", "preservedSurvivors")
	policy := replacementPolicyFixture()
	runtimeObj := replacementRuntimeFixture()
	oldNP := replacementOldNodeProvisionFixture()
	replacement := replacementReadyNodeProvisionFixture()
	migration := replacementPartialCheckpointFixture(true)
	reconciler := replacementReconcilerFixture(t, now, op, policy, runtimeObj, oldNP, replacement, migration)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("replacement reconcile create restore: %v", err)
	}
	restore := newRestoreRequest()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace-restore"}, restore); err != nil {
		t.Fatalf("restore request: %v", err)
	}
	restore.SetUID(types.UID("restore-uid"))
	restore.SetGeneration(1)
	assertProducedRestoreMatchesFixtureShape(t, restore)

	fixture := loadRestoreFixture(t)
	verification, _, _ := unstructured.NestedMap(fixture, "status", "verification")
	verification["requestUID"] = string(restore.GetUID())
	verification["trainingRuntimeRef"] = map[string]interface{}{"name": "runtime", "uid": "runtime-uid"}
	restore.Object["status"] = map[string]interface{}{"phase": "Verified", "observedGeneration": restore.GetGeneration(), "verification": verification}
	if err := reconciler.Update(context.Background(), restore); err != nil {
		t.Fatalf("update restore verification: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("replacement reconcile create cleanup: %v", err)
	}
	recovery := trainingpolicy.NewObject("SpotRecovery")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace-cleanup"}, recovery); err != nil {
		t.Fatalf("spot recovery cleanup: %v", err)
	}
	if oldMarket := stringField(recovery.Object, "spec", "oldMarketType"); oldMarket != "Spot" {
		t.Fatalf("cleanup oldMarketType = %q, want Spot", oldMarket)
	}
	if desiredMarket := stringField(recovery.Object, "spec", "desiredMarketType"); desiredMarket != "OnDemand" {
		t.Fatalf("cleanup desiredMarketType = %q, want OnDemand", desiredMarket)
	}
	if err := reconciler.Delete(context.Background(), oldNP); err != nil {
		t.Fatalf("delete old fixture node: %v", err)
	}
	recovery.Object["status"] = map[string]interface{}{"phase": "Completed"}
	if err := reconciler.Update(context.Background(), recovery); err != nil {
		t.Fatalf("update recovery completed: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("replacement reconcile after old deletion: %v", err)
	}
	updated := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace"}, updated); err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase != "Completed" {
		t.Fatalf("phase after cleanup = %q, want Completed", phase)
	}
}

func TestReplacementRestoreRequestFixtureMatchesProducedContract(t *testing.T) {
	fixture := loadRestoreFixture(t)
	if stringField(fixture, "spec", "sourceCluster") != stringField(fixture, "spec", "targetCluster") {
		t.Fatal("fixture must be same-cluster")
	}
	if boolField(fixture, "spec", "sourceFenced") {
		t.Fatal("fixture sourceFenced must be false; archive is not fencing proof")
	}
	if !boolField(fixture, "spec", "partialRestore", "preventPeriodicResume") {
		t.Fatal("fixture partialRestore.preventPeriodicResume missing")
	}
	if pods, ok, _ := unstructured.NestedSlice(fixture, "spec", "pods"); !ok || len(pods) == 0 {
		t.Fatal("fixture pods missing")
	} else {
		archives, ok, _ := unstructured.NestedSlice(pods[0].(map[string]interface{}), "archives")
		if !ok || len(archives) == 0 {
			t.Fatal("fixture archives missing")
		}
		archive := archives[0].(map[string]interface{})
		if stringField(archive, "sourcePath") == "" || stringField(archive, "targetPath") == "" || stringField(archive, "filePath") != "" || stringField(archive, "archiveEvidenceID") != "" {
			t.Fatalf("fixture RestoreRequest archive fields are not Stateful typed/pruning-safe: %#v", archive)
		}
	}
	verification, ok, _ := unstructured.NestedMap(fixture, "status", "verification")
	if !ok {
		t.Fatal("fixture status.verification missing")
	}
	if stringField(verification, "sourceFence", "evidenceID") == "" {
		t.Fatal("fixture must pin sourceFence evidence field expected after actuator proof")
	}
	targetRanks, ok, _ := unstructured.NestedSlice(verification, "partialRestore", "targetRanks")
	if !ok || len(targetRanks) == 0 || stringField(targetRanks[0].(map[string]interface{}), "archiveEvidenceID") == "" {
		t.Fatalf("fixture target rank archive evidence = %#v", targetRanks)
	}
	survivors, ok, _ := unstructured.NestedSlice(fixture, "spec", "partialRestore", "preservedSurvivors")
	if !ok || len(survivors) == 0 || !strings.HasSuffix(stringField(survivors[0].(map[string]interface{}), "pauseLockPath"), "/pause-lock") {
		t.Fatalf("fixture survivor pauseLockPath = %#v", survivors)
	}
}

func loadRestoreFixture(t *testing.T) map[string]interface{} {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "fixtures", "spot-replacement-restore-request.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture map[string]interface{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return fixture
}

func assertProducedRestoreMatchesFixtureShape(t *testing.T, restore *unstructured.Unstructured) {
	t.Helper()
	if stringField(restore.Object, "spec", "sourceCluster") != stringField(restore.Object, "spec", "targetCluster") {
		t.Fatal("produced RestoreRequest must be same-cluster")
	}
	if boolField(restore.Object, "spec", "sourceFenced") {
		t.Fatal("produced RestoreRequest sourceFenced must be false")
	}
	if !boolField(restore.Object, "spec", "partialRestore", "preventPeriodicResume") {
		t.Fatal("produced RestoreRequest missing preventPeriodicResume")
	}
	survivors, ok, _ := unstructured.NestedSlice(restore.Object, "spec", "partialRestore", "preservedSurvivors")
	if !ok || len(survivors) != 1 || stringField(survivors[0].(map[string]interface{}), "pauseLockPath") == "" {
		t.Fatalf("produced RestoreRequest survivor proof = %#v", survivors)
	}
	pods, ok, _ := unstructured.NestedSlice(restore.Object, "spec", "pods")
	if !ok || len(pods) != 1 {
		t.Fatalf("produced RestoreRequest pods = %#v", pods)
	}
	archives, ok, _ := unstructured.NestedSlice(pods[0].(map[string]interface{}), "archives")
	if !ok || len(archives) != 1 {
		t.Fatalf("produced RestoreRequest archive evidence = %#v", archives)
	}
	archive := archives[0].(map[string]interface{})
	if stringField(archive, "containerName") == "" || stringField(archive, "sourcePath") == "" || stringField(archive, "targetPath") == "" || stringField(archive, "sha256") == "" || stringField(archive, "durableRef") == "" {
		t.Fatalf("produced RestoreRequest archive evidence = %#v", archives)
	}
	if stringField(archive, "sourcePath") != "/var/lib/kubelet/checkpoints/checkpoint-trainer-1.tar" {
		t.Fatalf("sourcePath = %q", stringField(archive, "sourcePath"))
	}
	if stringField(archive, "targetPath") != "/var/lib/kubelet/checkpoints/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.tar" {
		t.Fatalf("targetPath = %q", stringField(archive, "targetPath"))
	}
	if stringField(archive, "filePath") != "" {
		t.Fatalf("produced RestoreRequest archive includes pruned filePath field: %#v", archive)
	}
	if stringField(archive, "archiveEvidenceID") != "" {
		t.Fatalf("produced RestoreRequest archive includes verification-only archiveEvidenceID field: %#v", archive)
	}
}

func TestReplacementReconcileEmitsTypedPartialCheckpointAfterReplacementReady(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	policy := replacementPolicyFixture()
	oldNP := replacementOldNodeProvisionFixture()
	replacement := replacementReadyNodeProvisionFixture()
	reconciler := replacementReconcilerFixture(t, now, op, policy, oldNP, replacement)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	migration := trainingpolicy.NewObject("FluidCRMigration")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace-partial-checkpoint"}, migration); err != nil {
		t.Fatalf("partial checkpoint: %v", err)
	}
	if resume := boolField(migration.Object, "spec", "resume"); resume {
		t.Fatal("partial checkpoint emitted resume=true")
	}
	ranks, ok, _ := unstructured.NestedSlice(migration.Object, "spec", "partialCheckpoint", "targetRanks")
	if !ok || len(ranks) != 1 || ranks[0].(int64) != 1 {
		t.Fatalf("targetRanks = %#v, want [1]", ranks)
	}
	if _, ok, _ := unstructured.NestedBool(migration.Object, "spec", "partialRankCheckpointRequired"); ok {
		t.Fatal("emitted stale partialRankCheckpointRequired field")
	}
	if _, ok, _ := unstructured.NestedBool(migration.Object, "spec", "noPeriodicResume"); ok {
		t.Fatal("emitted stale noPeriodicResume field")
	}
}

func TestReplacementReconcileRejectsMissingSourcePodUID(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	pods := []interface{}{map[string]interface{}{"rank": int64(1), "sourcePod": "trainer-1", "sourceNode": "old-node"}}
	_ = unstructured.SetNestedSlice(op.Object, pods, "spec", "pods")
	reconciler := replacementReconcilerFixture(t, now, op, replacementPolicyFixture(), replacementOldNodeProvisionFixture())

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	updated := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace"}, updated); err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase != "Rejected" {
		t.Fatalf("phase = %q, want Rejected", phase)
	}
	if msg := stringField(updated.Object, "status", "message"); !strings.Contains(msg, "sourcePodUID") {
		t.Fatalf("message = %q, want sourcePodUID rejection", msg)
	}
}

func TestReplacementReconcileRejectsRankZeroBeforeCapacity(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	op := replacementOperationFixture()
	_ = unstructured.SetNestedSlice(op.Object, []interface{}{int64(0)}, "spec", "partialCheckpoint", "targetRanks")
	_ = unstructured.SetNestedSlice(op.Object, []interface{}{map[string]interface{}{"rank": int64(0), "sourcePod": "trainer-0", "sourcePodUID": "old-pod-uid", "sourceNode": "old-node", "targetNode": "new-node"}}, "spec", "pods")
	reconciler := replacementReconcilerFixture(t, now, op, replacementPolicyFixture(), replacementOldNodeProvisionFixture())

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "old-replace"}}); err != nil {
		t.Fatalf("replacement reconcile: %v", err)
	}
	updated := newSpotReplacementObject()
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old-replace"}, updated); err != nil {
		t.Fatalf("get operation: %v", err)
	}
	if msg := stringField(updated.Object, "status", "message"); !strings.Contains(msg, "UnsupportedRankZero") {
		t.Fatalf("message = %q, want UnsupportedRankZero", msg)
	}
	replacement := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "new"}, replacement); err == nil {
		t.Fatal("rank-0 replacement created capacity before rejection")
	}
}

func TestReplacementCleanupOmitsAbsentEventID(t *testing.T) {
	for _, eventID := range []string{"", "notice-1"} {
		t.Run("event-"+eventID, func(t *testing.T) {
			r := replacementReconcilerFixture(t, time.Now())
			obj, created, err := r.ensureSpotRecovery(context.Background(), "default", replacementSpec{}, recoverySpec{
				Operation: "replacement", EventID: eventID,
			})
			if err != nil || !created {
				t.Fatalf("create cleanup: created=%v err=%v", created, err)
			}
			value, found, err := unstructured.NestedString(obj.Object, "spec", "eventID")
			if err != nil || value != eventID || found != (eventID != "") {
				t.Fatalf("eventID=%q present=%v err=%v; empty eventID violates CRD minLength", value, found, err)
			}
		})
	}
}

func replacementReconcilerFixture(t *testing.T, now time.Time, objects ...client.Object) *ReplacementReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, gvk := range []string{"TrainingPolicy", "TrainingRuntime", "SpotRecovery", "NodeProvision", "FluidCRMigration", "PropagationPolicy"} {
		obj := trainingpolicy.NewObject(gvk)
		scheme.AddKnownTypeWithName(obj.GroupVersionKind(), &unstructured.Unstructured{})
		listGVK := obj.GroupVersionKind()
		listGVK.Kind += "List"
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	scheme.AddKnownTypeWithName(spotReplacementGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(spotReplacementGVK.GroupVersion().WithKind("SpotReplacementList"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(restoreRequestGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(restoreRequestGVK.GroupVersion().WithKind("RestoreRequestList"), &unstructured.UnstructuredList{})
	statusObjects := []client.Object{newSpotReplacementObject()}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(statusObjects...).WithObjects(objects...).Build()
	return &ReplacementReconciler{Client: c, APIReader: c, Clock: func() time.Time { return now }}
}

func replacementOperationFixture() *unstructured.Unstructured {
	op := newSpotReplacementObject()
	op.SetNamespace("default")
	op.SetName("old-replace")
	op.SetUID(types.UID("operation-uid"))
	op.SetGeneration(1)
	op.Object["spec"] = map[string]interface{}{
		"operation":                   "old-replace",
		"policyRef":                   map[string]interface{}{"name": "policy", "uid": "policy-uid", "generation": int64(3)},
		"workloadRef":                 map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload-uid"},
		"sourceCluster":               "aws",
		"targetCluster":               "aws",
		"oldNodeProvisionRef":         map[string]interface{}{"name": "old", "uid": "old-uid"},
		"replacementNodeProvisionRef": map[string]interface{}{"name": "new"},
		"desiredMarketType":           "OnDemand",
		"partialCheckpoint":           map[string]interface{}{"targetRanks": []interface{}{int64(1)}},
		"pods":                        []interface{}{map[string]interface{}{"rank": int64(1), "sourcePod": "trainer-1", "sourcePodUID": "old-pod-uid", "sourceNode": "old-node"}},
		"partialRestore":              map[string]interface{}{"preventPeriodicResume": true, "targetRanks": []interface{}{int64(1)}},
	}
	return op
}

func replacementPolicyFixture() *unstructured.Unstructured {
	policy := trainingpolicy.NewObject("TrainingPolicy")
	policy.SetNamespace("default")
	policy.SetName("policy")
	policy.SetUID(types.UID("policy-uid"))
	policy.SetGeneration(3)
	policy.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}, "runtimeRef": map[string]interface{}{"name": "runtime"}}
	return policy
}

func replacementOldNodeProvisionFixture() *unstructured.Unstructured {
	np := trainingpolicy.NewObject("NodeProvision")
	np.SetNamespace("default")
	np.SetName("old")
	np.SetUID(types.UID("old-uid"))
	np.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy-uid"})
	np.Object["spec"] = map[string]interface{}{"marketType": "Spot", "hostname": "old", "instanceType": "m5.large"}
	np.Object["status"] = map[string]interface{}{"spot": map[string]interface{}{"eventID": "event-1"}}
	return np
}

func setEmergencyReplacementSpotEvidence(np *unstructured.Unstructured, eventID string) {
	np.Object["status"] = map[string]interface{}{
		"instanceId": "i-old",
		"nodeName":   "old-node",
		"spot": map[string]interface{}{
			"atRisk":     true,
			"signalType": "InterruptionNotice",
			"eventID":    eventID,
			"instanceID": "i-old",
		},
	}
}

func replacementReadyNodeProvisionFixture() *unstructured.Unstructured {
	np := trainingpolicy.NewObject("NodeProvision")
	np.SetNamespace("default")
	np.SetName("new")
	np.SetUID(types.UID("new-uid"))
	np.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy-uid", trainingpolicy.LabelRole: "replacement"})
	np.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": "old-replace"})
	np.Object["spec"] = map[string]interface{}{"marketType": "OnDemand", "hostname": "new", "instanceType": "m5.large"}
	np.Object["status"] = map[string]interface{}{"phase": "Ready", "instanceId": "i-new", "nodeName": "new-node"}
	return np
}

func replacementRuntimeFixture() *unstructured.Unstructured {
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	runtimeObj.SetNamespace("default")
	runtimeObj.SetName("runtime")
	runtimeObj.SetUID(types.UID("runtime-uid"))
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
	return runtimeObj
}

func replacementPartialCheckpointFixture(completed bool) *unstructured.Unstructured {
	migration := trainingpolicy.NewObject("FluidCRMigration")
	migration.SetNamespace("default")
	migration.SetName("old-replace-partial-checkpoint")
	migration.SetUID(types.UID("checkpoint-uid"))
	migration.SetGeneration(3)
	migration.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy-uid", trainingpolicy.LabelRole: "replacement-checkpoint"})
	migration.SetAnnotations(map[string]string{"training.dcnlab.com/checkpoint-id": "old-replace-partial-checkpoint"})
	migration.Object["spec"] = map[string]interface{}{"resume": false, "partialCheckpoint": map[string]interface{}{"targetRanks": []interface{}{int64(1)}}}
	phase := "Running"
	status := map[string]interface{}{"clusterName": "aws", "phase": phase, "observedGeneration": int64(3)}
	if completed {
		phase = "Completed"
		status["phase"] = phase
		status["status"] = map[string]interface{}{"pods": []interface{}{
			map[string]interface{}{
				"rank":         int64(1),
				"podName":      "trainer-1",
				"podUID":       "old-pod-uid",
				"phase":        "ContainerCheckpointed",
				"checkpointID": "old-replace-partial-checkpoint",
				"checkpointFiles": []interface{}{map[string]interface{}{
					"containerName": "trainer",
					"filePath":      "/var/lib/kubelet/checkpoints/checkpoint-trainer-1.tar",
					"sha256":        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"durableRef":    "file-store:default/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"checkpointID":  "old-replace-partial-checkpoint",
				}},
			},
			map[string]interface{}{
				"rank":    int64(0),
				"podName": "trainer-0",
				"podUID":  "survivor-pod-uid",
				"phase":   "SurvivorPaused",
				"survivorEvidence": map[string]interface{}{
					"generation":    int64(9),
					"pauseLockPath": "/var/lib/fluidcr/pause-locks/trainer-0/pause-lock",
					"observedAt":    "2026-09-26T00:00:00Z",
				},
			},
		}}
	}
	migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{status}}
	return migration
}
