package management

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPartialFallbackDoesNotTriggerOnFailedCheckpointAlone(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, migration, _, _ := replacementFallbackFixtures(t, now, false)
	markReplacementPartialFailed(migration)
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, migration)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase == replacementPhaseAwaitingGroupFallback {
		t.Fatalf("Failed partial checkpoint alone triggered group fallback: %#v", updated.Object["status"])
	}
	if _, ok, _ := unstructured.NestedMap(updated.Object, "status", "partialFallback"); ok {
		t.Fatalf("unexpected partialFallback status: %#v", updated.Object["status"])
	}
	group := newRestoreRequest()
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "operation-group-restore"}, group); !apierrors.IsNotFound(err) {
		t.Fatalf("group restore created from Failed alone: %v", err)
	}
}

func TestPartialFallbackDoesNotInferFromStaleRuntime(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, migration, _, _ := replacementFallbackFixtures(t, now, true)
	markReplacementPartialFailed(migration)
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, migration)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase == replacementPhaseAwaitingGroupFallback {
		t.Fatalf("stale runtime inferred partial impossibility: %#v", updated.Object["status"])
	}
}

func TestPartialFallbackRecordsHandoffBeforeGroupRestore(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	fallback, ok, _ := unstructured.NestedMap(updated.Object, "status", "partialFallback")
	if !ok || stringField(fallback, "reason") != "target_source_pod_replaced" || stringField(fallback, "checkpointRef", "uid") != "checkpoint" {
		t.Fatalf("fallback handoff = %#v status=%#v", fallback, updated.Object["status"])
	}
	group := newRestoreRequest()
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "operation-group-restore"}, group); !apierrors.IsNotFound(err) {
		t.Fatalf("group restore must wait until a recorded handoff reconcile: %v", err)
	}
}

func TestPartialFallbackRecordsFailedPartialOnlyWhenAllOriginalsReplaced(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, migration, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntimeAll(runtimeObj, now, "trainer-0-CURRENT", "trainer-1-CURRENT")
	markReplacementPartialFailedWithOriginalPods(migration)
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, migration, checkpoint, replacementFallbackSurvivorNode(), sts)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	fallback, ok, _ := unstructured.NestedMap(updated.Object, "status", "partialFallback")
	if !ok || stringField(fallback, "reason") != "failed_partial_all_originals_replaced" || stringField(fallback, "failedPartialCheckpointRef", "uid") != "partial-uid" || intField(fallback, "failedPartialCheckpointRef", "generation") != int64(1) {
		t.Fatalf("failed partial fallback handoff = %#v status=%#v", fallback, updated.Object["status"])
	}
}
func TestPartialFallbackCreatesGroupRestoreAfterRecordedHandoff(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: "np-origin-1"})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "np-origin-1", "reason": "group_intent_quiesced"}}
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
		t.Fatal(err)
	}
	group := newRestoreRequest()
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "operation-group-restore"}, group); err != nil {
		t.Fatalf("group restore request: %v", err)
	}
	if stringField(group.Object, "spec", "checkpointRef", "uid") != "checkpoint" || stringField(group.Object, "spec", "groupRestore", "operationUID") != "np-origin-1" {
		t.Fatalf("group restore did not preserve fallback checkpoint/operation: %#v", group.Object["spec"])
	}
	if strings.Contains(group.GetName(), "-restore-restore") {
		t.Fatalf("unexpected restore name collision: %s", group.GetName())
	}
	patchedReplacement := trainingpolicy.NewObject("NodeProvision")
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "replacement"}, patchedReplacement); err != nil {
		t.Fatal(err)
	}
	if patchedReplacement.GetAnnotations()[groupOldUID] != "np-origin-1" {
		t.Fatalf("replacement was not prepared for group producer reuse: %#v", patchedReplacement.GetAnnotations())
	}
}
func TestPartialFallbackRecordedHandoffRejectsLatePartialCheckpoint(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, migration, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: "np-origin-1"})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "np-origin-1", "reason": "group_intent_quiesced"}}
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(context.Background(), migration); err != nil {
		t.Fatalf("late partial checkpoint create: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if msg := stringField(updated.Object, "status", "message"); !strings.Contains(msg, "partial checkpoint exists after fallback handoff") {
		t.Fatalf("message = %q status=%#v", msg, updated.Object["status"])
	}
	assertNoFallbackGroupRestore(t, r)
}

func TestPartialFallbackRecordedHandoffHoldsOnPartialCheckpointAPIError(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: "np-origin-1"})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "np-origin-1", "reason": "group_intent_quiesced"}}
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	r.APIReader = failingGetReader{Reader: r.Client, kind: "FluidCRMigration", name: "operation-partial-checkpoint"}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if msg := stringField(updated.Object, "status", "message"); !strings.Contains(msg, "api timeout") {
		t.Fatalf("message = %q status=%#v", msg, updated.Object["status"])
	}
	assertNoFallbackGroupRestore(t, r)
}

func TestPartialFallbackRecordedHandoffFailsClosedWhenPinnedCheckpointIsNoLongerSelected(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: "np-origin-1"})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "np-origin-1", "reason": "group_intent_quiesced"}}
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	newer := checkpoint.DeepCopy()
	newer.SetName("newer-round")
	newer.SetResourceVersion("")
	newer.SetUID(types.UID("newer-checkpoint"))
	newer.SetAnnotations(map[string]string{"training.dcnlab.com/checkpoint-id": "newer-round"})
	groupReport(newer)["completionTime"] = "2026-09-28T00:03:00Z"
	if err := r.Create(context.Background(), newer); err != nil {
		t.Fatalf("newer checkpoint create: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if msg := stringField(updated.Object, "status", "message"); !strings.Contains(msg, "durable full-group checkpoint changed") {
		t.Fatalf("message = %q status=%#v", msg, updated.Object["status"])
	}
	assertNoFallbackGroupRestore(t, r)
}

func TestPartialFallbackRecordedHandoffRevalidatesFreshLossProof(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: "np-origin-1"})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "np-origin-1", "reason": "group_intent_quiesced"}}
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtimeObj), runtimeObj); err != nil {
		t.Fatal(err)
	}
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-uid")
	if err := r.Update(context.Background(), runtimeObj); err != nil {
		t.Fatalf("restore runtime original UID: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if msg := stringField(updated.Object, "status", "message"); !strings.Contains(msg, "fresh source snapshot no longer proves") {
		t.Fatalf("message = %q status=%#v", msg, updated.Object["status"])
	}
	assertNoFallbackGroupRestore(t, r)
}
func TestPartialFallbackGroupVerifiedWaitsForCleanupEvidence(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: "np-origin-1"})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "np-origin-1", "reason": "group_intent_quiesced"}}
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	group := newRestoreRequest()
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "operation-group-restore"}, group); err != nil {
		t.Fatalf("group restore request: %v", err)
	}
	group.SetUID(types.UID("group-restore-uid"))
	group.SetGeneration(1)
	group.Object["status"] = map[string]interface{}{
		"phase":              "Verified",
		"observedGeneration": int64(1),
		"verification": map[string]interface{}{
			"requestUID":         "group-restore-uid",
			"operation":          "np-origin-1",
			"checkpointID":       "round",
			"sourceCluster":      "aws",
			"targetCluster":      "aws",
			"trainingRuntimeRef": map[string]interface{}{"name": "runtime", "uid": "runtime-uid"},
			"verifiedAt":         now.Format(time.RFC3339),
			"sourceFenced":       true,
			"sourceFence":        map[string]interface{}{"fenced": true, "operation": "np-origin-1", "evidenceID": "fence-1", "observedAt": now.Format(time.RFC3339)},
		},
	}
	if err := r.Update(context.Background(), group); err != nil {
		t.Fatalf("update group verification: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase == "Completed" {
		t.Fatalf("completed before cleanup evidence: %#v", updated.Object["status"])
	}
	recovery := trainingpolicy.NewObject("SpotRecovery")
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "operation-cleanup"}, recovery); err != nil {
		t.Fatalf("cleanup not created: %v", err)
	}
	recovery.Object["status"] = map[string]interface{}{"phase": "Completed"}
	if err := r.Update(context.Background(), recovery); err != nil {
		t.Fatalf("update cleanup: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase != "Completed" {
		t.Fatalf("phase = %q status=%#v", phase, updated.Object["status"])
	}
}
func TestPartialFallbackHoldsWhilePartialCheckpointInProgress(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, migration, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, migration, checkpoint, replacementFallbackSurvivorNode(), sts)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
		t.Fatal(err)
	}
	updated := newSpotReplacementObject()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), updated); err != nil {
		t.Fatal(err)
	}
	if phase := stringField(updated.Object, "status", "phase"); phase == replacementPhaseAwaitingGroupFallback {
		t.Fatalf("in-progress partial checkpoint triggered group fallback: %#v", updated.Object["status"])
	}
}

func TestPartialFallbackDoesNotTreatAPIErrorAsAbsentRestore(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)
	r.APIReader = failingGetReader{Reader: r.Client, kind: "RestoreRequest", name: "operation-restore"}
	_, eligible, err := r.classifyPartialToGroupFallback(context.Background(), "demo", mustReplacementSpec(t, op))
	if err == nil || !strings.Contains(err.Error(), "api timeout") || eligible {
		t.Fatalf("eligible=%v err=%v, want API error without fallback", eligible, err)
	}
}

func assertNoFallbackGroupRestore(t *testing.T, r *ReplacementReconciler) {
	t.Helper()
	group := newRestoreRequest()
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "operation-group-restore"}, group); !apierrors.IsNotFound(err) {
		t.Fatalf("group restore exists/lookup err = %v object=%#v", err, group.Object)
	}
}

type failingGetReader struct {
	client.Reader
	kind string
	name string
}

func (r failingGetReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == r.kind && key.Name == r.name {
		return fmt.Errorf("api timeout reading %s/%s", key.Namespace, key.Name)
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func mustReplacementSpec(t *testing.T, op *unstructured.Unstructured) replacementSpec {
	t.Helper()
	spec, err := readReplacementSpec(op)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
func replacementFallbackReconcilerFixture(t *testing.T, now time.Time, objects ...client.Object) *ReplacementReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, gvk := range []string{"TrainingPolicy", "TrainingRuntime", "SpotRecovery", "NodeProvision", "FluidCRMigration", "PropagationPolicy", "StatefulSet"} {
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
func replacementFallbackFixtures(t *testing.T, now time.Time, stale bool) (*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	op := newSpotReplacementObject()
	op.SetNamespace("demo")
	op.SetName("operation")
	op.SetUID(types.UID("operation-uid"))
	op.SetGeneration(1)
	op.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy"})
	op.Object["spec"] = map[string]interface{}{
		"operation":                   "operation",
		"policyRef":                   map[string]interface{}{"name": "policy", "uid": "policy", "generation": int64(1)},
		"workloadRef":                 map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload"},
		"sourceCluster":               "aws",
		"targetCluster":               "aws",
		"oldNodeProvisionRef":         map[string]interface{}{"name": "np-1", "uid": "np-origin-1"},
		"replacementNodeProvisionRef": map[string]interface{}{"name": "replacement"},
		"desiredMarketType":           "OnDemand",
		"partialCheckpoint":           map[string]interface{}{"targetRanks": []interface{}{int64(1)}},
		"pods":                        []interface{}{map[string]interface{}{"rank": int64(1), "sourcePod": "trainer-1", "sourcePodUID": "trainer-1-uid", "sourceNode": "trainer-1-node"}},
		"partialRestore":              map[string]interface{}{"preventPeriodicResume": true, "targetRanks": []interface{}{int64(1)}, "preservedSurvivors": []interface{}{map[string]interface{}{"rank": int64(0), "podName": "trainer-0", "podUID": "trainer-0-uid", "nodeName": "trainer-0-node"}}},
	}
	policy := trainingpolicy.NewObject("TrainingPolicy")
	policy.SetNamespace("demo")
	policy.SetName("policy")
	policy.SetUID(types.UID("policy"))
	policy.SetGeneration(1)
	policy.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload"}, "sourceCluster": "aws", "targetWorkers": int64(2), "runtimeRef": map[string]interface{}{"name": "runtime"}}
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	runtimeObj.SetNamespace("demo")
	runtimeObj.SetName("runtime")
	runtimeObj.SetUID(types.UID("runtime-uid"))
	runtimeObj.SetGeneration(1)
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload"}, "sourceCluster": "aws", "expectedWorldSize": int64(2)}
	observed := now
	if stale {
		observed = now.Add(-3 * time.Minute)
	}
	refreshFallbackRuntime(runtimeObj, observed, "trainer-1-uid")
	old := trainingpolicy.NewObject("NodeProvision")
	old.SetNamespace("demo")
	old.SetName("np-1")
	old.SetUID(types.UID("np-origin-1"))
	old.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy"})
	old.Object["spec"] = map[string]interface{}{"marketType": "Spot", "hostname": "trainer-1-node"}
	old.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": "trainer-1-node", "instanceId": "i-1", "memberUID": "member-1"}
	replacement := trainingpolicy.NewObject("NodeProvision")
	replacement.SetNamespace("demo")
	replacement.SetName("replacement")
	replacement.SetUID(types.UID("replacement-uid"))
	replacement.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy", trainingpolicy.LabelRole: "replacement"})
	replacement.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": "operation"})
	replacement.Object["spec"] = map[string]interface{}{"marketType": "OnDemand", "hostname": "replacement"}
	replacement.Object["status"] = map[string]interface{}{"phase": "Ready", "instanceId": "i-new", "nodeName": "new-node"}
	migration := trainingpolicy.NewObject("FluidCRMigration")
	migration.SetNamespace("demo")
	migration.SetName("operation-partial-checkpoint")
	migration.SetUID(types.UID("partial-uid"))
	migration.SetGeneration(1)
	migration.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy", trainingpolicy.LabelRole: "replacement-checkpoint"})
	migration.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": "operation", "training.dcnlab.com/recovery-operation-uid": "operation-uid", "training.dcnlab.com/checkpoint-id": "operation-partial-checkpoint"})
	migration.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload"}, "resume": false, "partialCheckpoint": map[string]interface{}{"targetRanks": []interface{}{int64(1)}}}
	migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "phase": "Running", "observedGeneration": int64(1)}}}
	checkpoint, _ := groupCheckpointFixture()
	sts := trainingpolicy.NewObject("StatefulSet")
	sts.SetNamespace("demo")
	sts.SetName("trainer")
	sts.SetUID(types.UID("workload"))
	sts.Object["spec"] = map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "trainer", "volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}}}}, "volumes": []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}}}}}}
	return op, policy, runtimeObj, old, replacement, migration, checkpoint, sts
}

func refreshFallbackRuntime(runtimeObj *unstructured.Unstructured, observed time.Time, rank1UID string) {
	refreshFallbackRuntimeAll(runtimeObj, observed, "trainer-0-uid", rank1UID)
}

func refreshFallbackRuntimeAll(runtimeObj *unstructured.Unstructured, observed time.Time, rank0UID, rank1UID string) {
	sourcePods := []interface{}{
		map[string]interface{}{"name": "trainer-0", "uid": rank0UID, "rank": int64(0), "nodeName": "trainer-0-node"},
		map[string]interface{}{"name": "trainer-1", "uid": rank1UID, "rank": int64(1), "nodeName": "trainer-1-node"},
	}
	pods := []interface{}{
		map[string]interface{}{"name": "trainer-0", "uid": rank0UID, "rank": int64(0), "observedAt": observed.Format(time.RFC3339)},
		map[string]interface{}{"name": "trainer-1", "uid": rank1UID, "rank": int64(1), "observedAt": observed.Format(time.RFC3339)},
	}
	runtimeObj.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "status": map[string]interface{}{"observedGeneration": int64(1), "sourceWorldUID": "workload", "sourcePods": sourcePods, "phase": "Running", "workloadUID": "workload", "memberWorkloadUID": "member-world", "readyRanks": int64(2), "worldSize": int64(2), "observedAt": observed.Format(time.RFC3339), "pods": pods}}}}
}
func replacementFallbackSurvivorNode() *unstructured.Unstructured {
	np := trainingpolicy.NewObject("NodeProvision")
	np.SetNamespace("demo")
	np.SetName("np-0")
	np.SetUID(types.UID("np-origin-0"))
	np.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy"})
	np.Object["spec"] = map[string]interface{}{"marketType": "Spot", "hostname": "trainer-0-node"}
	np.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": "trainer-0-node", "instanceId": "i-0", "memberUID": "member-0"}
	return np
}

func markReplacementPartialFailed(migration *unstructured.Unstructured) {
	migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "phase": "Failed", "observedGeneration": int64(1), "message": "member timeout"}}}
}

func markReplacementPartialFailedWithOriginalPods(migration *unstructured.Unstructured) {
	migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "phase": "Failed", "observedGeneration": int64(1), "message": "all originals replaced", "status": map[string]interface{}{"pods": []interface{}{
		map[string]interface{}{"rank": int64(0), "podName": "trainer-0", "podUID": "trainer-0-uid"},
		map[string]interface{}{"rank": int64(1), "podName": "trainer-1", "podUID": "trainer-1-uid"},
	}}}}}
}
