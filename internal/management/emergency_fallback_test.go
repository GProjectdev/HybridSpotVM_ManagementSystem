package management

import (
	"context"
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

func emergencyFallbackFixtures(t *testing.T, now time.Time) (*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	op, policy, runtime, old, replacement, partial, full, sts := replacementFallbackFixtures(t, now, true)
	operation := replacementOperationName(old.GetName(), string(old.GetUID()))
	op.SetName(operation)
	_ = unstructured.SetNestedField(op.Object, operation, "spec", "operation")
	old.Object["status"].(map[string]interface{})["spot"] = map[string]interface{}{"eventID": "event", "instanceID": "i-1", "atRisk": true, "signalType": spotInterruptionNotice, "noticeTime": now.Add(-3 * time.Minute).Format(time.RFC3339), "interruptionTime": now.Add(-time.Second).Format(time.RFC3339)}
	a := emergencyBinding(old)
	a[annotationEmergencyEventID] = "event"
	op.SetAnnotations(a)
	partial.SetName(operation + "-partial-checkpoint")
	partial.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": operation, "training.dcnlab.com/recovery-operation-uid": string(op.GetUID()), "training.dcnlab.com/checkpoint-id": partial.GetName()})
	labels := partial.GetLabels()
	labels[p.LabelManagedBy] = "hybridspotvm-system"
	partial.SetLabels(labels)
	ra := replacement.GetAnnotations()
	ra["training.dcnlab.com/recovery-operation"] = operation
	replacement.SetAnnotations(ra)
	report := runtime.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})["status"].(map[string]interface{})
	report["phase"], report["readyRanks"] = "Unavailable", int64(0)
	return op, policy, runtime, old, replacement, partial, full, sts
}

func TestEmergencyFallbackDeadlineAndOwnership(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	for _, scenario := range []string{"pending", "failed", "before-deadline", "rebalance", "no-checkpoint", "source-changed", "partial-restore-exists", "instance-changed"} {
		t.Run(scenario, func(t *testing.T) {
			op, policy, runtime, old, replacement, partial, full, sts := emergencyFallbackFixtures(t, now)
			spot := old.Object["status"].(map[string]interface{})["spot"].(map[string]interface{})
			objects := []client.Object{op, policy, runtime, old, replacement, partial, replacementFallbackSurvivorNode(), sts}
			switch scenario {
			case "failed":
				markReplacementPartialFailed(partial)
			case "before-deadline":
				spot["interruptionTime"] = now.Add(time.Minute).Format(time.RFC3339)
			case "rebalance":
				spot["signalType"] = spotRebalanceRecommendation
				a := op.GetAnnotations()
				a[annotationEmergencySignal] = spotRebalanceRecommendation
				op.SetAnnotations(a)
			case "source-changed":
				refreshFallbackRuntime(runtime, now, "unexpected-uid")
			case "instance-changed":
				old.Object["status"].(map[string]interface{})["instanceId"] = "i-other"
			case "partial-restore-exists":
				request := newRestoreRequest()
				request.SetNamespace("demo")
				request.SetName(op.GetName() + "-restore")
				objects = append(objects, request)
			}
			if scenario != "no-checkpoint" {
				objects = append(objects, full)
			}
			r := replacementFallbackReconcilerFixture(t, now, objects...)
			spec, err := readReplacementSpec(op)
			if err != nil {
				t.Fatal(err)
			}
			d, eligible, err := r.classifyPartialToGroupFallback(context.Background(), "demo", spec)
			if scenario == "pending" || scenario == "failed" {
				if err != nil || !eligible || d.Reason != emergencyFallbackReason || len(d.EmergencySources) != 2 || stringField(d.CheckpointRef, "uid") != "checkpoint" {
					t.Fatalf("decision=%+v eligible=%v err=%v", d, eligible, err)
				}
			} else if err == nil && eligible {
				t.Fatalf("unsafe fallback accepted: %+v", d)
			}
		})
	}
}

func TestEmergencyFallbackWaitsForMemberAndPinsRound(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtime, old, replacement, partial, full, sts := emergencyFallbackFixtures(t, now)
	policy.SetAnnotations(map[string]string{groupIntentAnnotation: string(old.GetUID())})
	policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": string(old.GetUID()), "reason": "group_intent_quiesced"}}
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtime, old, replacement, partial, full, replacementFallbackSurvivorNode(), sts)
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(op), op); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	fallback, found, _ := unstructured.NestedMap(op.Object, "status", "partialFallback")
	if !found || !emergencyFallback(fallback) {
		t.Fatalf("handoff missing: %#v", op.Object["status"])
	}
	reconcile()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(partial), partial); err != nil {
		t.Fatal(err)
	}
	if partial.GetAnnotations()[groupFallbackCancelAnnotation] != string(op.GetUID()) {
		t.Fatal("member cancellation not requested")
	}
	group := newRestoreRequest()
	key := client.ObjectKey{Namespace: "demo", Name: op.GetName() + "-group-restore"}
	if err := r.Get(context.Background(), key, group); !apierrors.IsNotFound(err) {
		t.Fatal("group created before cancellation acknowledgement")
	}
	markReplacementPartialFailed(partial)
	if err := r.Update(context.Background(), partial); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if err := r.Get(context.Background(), key, group); !apierrors.IsNotFound(err) {
		t.Fatal("ordinary failure treated as cancellation acknowledgement")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(partial), partial); err != nil {
		t.Fatal(err)
	}
	partial.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "phase": "Failed", "observedGeneration": partial.GetGeneration(), "message": "group fallback checkpoint cancelled: " + string(op.GetUID())}}}
	if err := r.Update(context.Background(), partial); err != nil {
		t.Fatal(err)
	}
	newer := full.DeepCopy()
	newer.SetName("newer")
	newer.SetUID(types.UID("newer-uid"))
	newer.SetResourceVersion("")
	newer.SetAnnotations(map[string]string{"training.dcnlab.com/checkpoint-id": "newer"})
	groupReport(newer)["completionTime"] = now.Add(time.Minute).Format(time.RFC3339)
	if err := r.Create(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if err := r.Get(context.Background(), key, group); err != nil {
		t.Fatalf("group missing: %v; status=%#v", err, op.Object["status"])
	}
	if stringField(group.Object, "spec", "checkpointRef", "uid") != string(full.GetUID()) || group.GetAnnotations()[groupCleanupOwnerUID] != string(op.GetUID()) {
		t.Fatal("group switched pinned round or cleanup owner")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(old), old); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedMap(old.Object, map[string]interface{}{"operationUID": string(old.GetUID()), "instanceID": "i-1"}, "spec", "fence")
	if err := r.Update(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if stringField(op.Object, "status", "phase") == "Rejected" || !strings.Contains(stringField(op.Object, "status", "message"), "verification") {
		t.Fatalf("own provider fence rejected: %#v", op.Object["status"])
	}
}
