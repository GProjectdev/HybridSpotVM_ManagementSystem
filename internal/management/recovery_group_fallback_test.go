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

func TestEmergencyFallbackPreservesValidCompletedPartial(t *testing.T) {
	ctx := context.Background()
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, partial, full, sts := emergencyFallbackFixtures(t, now)
	completed := replacementPartialCheckpointFixture(true)
	report, ok := sourceClusterStatus(completed, "aws")
	if !ok {
		t.Fatal("completed fixture report missing")
	}
	report["observedGeneration"] = partial.GetGeneration()
	pods, ok := mapSliceFromStatus(report, "pods")
	if !ok {
		t.Fatal("completed fixture pods missing")
	}
	for _, pod := range pods {
		if intField(pod, "rank") == 1 {
			pod["podUID"] = "trainer-1-uid"
			pod["checkpointID"] = partial.GetName()
			files := pod["checkpointFiles"].([]interface{})
			for _, raw := range files {
				raw.(map[string]interface{})["checkpointID"] = partial.GetName()
			}
		} else {
			pod["podUID"] = "trainer-0-uid"
		}
	}
	rawPods := make([]interface{}, len(pods))
	for i := range pods {
		rawPods[i] = pods[i]
	}
	report["pods"] = rawPods
	partial.Object["status"] = map[string]interface{}{"clusters": []interface{}{report}}
	spec, err := readReplacementSpec(op)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPartialCheckpointEvidence(partial, spec); err != nil {
		t.Fatalf("fixture is not a usable partial: %v", err)
	}
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, partial, full, sts, replacementFallbackSurvivorNode())
	_, eligible, err := r.classifyPartialToGroupFallback(ctx, "demo", spec)
	if err != nil || eligible {
		t.Fatalf("usable partial discarded: eligible=%v err=%v", eligible, err)
	}
	got := p.NewObject("FluidCRMigration")
	if err := r.Get(ctx, client.ObjectKeyFromObject(partial), got); err != nil {
		t.Fatal(err)
	}
	if got.GetAnnotations()[groupFallbackCancelAnnotation] != "" {
		t.Fatal("completed partial cancellation requested")
	}
}

func TestGroupFallbackRealRecoveryCleanup(t *testing.T) {
	for _, scenario := range []string{"valid", "owner-uid", "fence-operation", "checkpoint", "missing-handoff", "source-identities", "volumes"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := mustParseTime(t, "2026-09-28T00:02:00Z")
			op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
			refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
			_ = unstructured.SetNestedField(old.Object, "aws", "status", "observedCluster")
			policy.SetAnnotations(map[string]string{groupIntentAnnotation: string(old.GetUID())})
			policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": string(old.GetUID()), "reason": "group_intent_quiesced"}}
			r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)
			key := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}
			for i := 0; i < 2; i++ {
				if _, err := r.Reconcile(ctx, key); err != nil {
					t.Fatal(err)
				}
			}
			group := newRestoreRequest()
			if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "operation-group-restore"}, group); err != nil {
				t.Fatal(err)
			}
			group.SetUID(types.UID("group-uid"))
			group.SetGeneration(1)
			group.Object["status"] = map[string]interface{}{"phase": "Verified", "observedGeneration": int64(1), "verification": map[string]interface{}{
				"requestUID": "group-uid", "operation": string(old.GetUID()), "checkpointID": "round", "sourceCluster": "aws", "targetCluster": "aws",
				"trainingRuntimeRef": map[string]interface{}{"name": "runtime", "uid": "runtime-uid"}, "verifiedAt": now.Format(time.RFC3339), "sourceFenced": true,
				"sourceFence": map[string]interface{}{"fenced": true, "operation": string(old.GetUID()), "evidenceID": "fence-1", "observedAt": now.Format(time.RFC3339)},
			}}
			// Archive provenance is deliberately different from the fenced current world.
			pods, _, _ := unstructured.NestedSlice(group.Object, "spec", "pods")
			for _, raw := range pods {
				raw.(map[string]interface{})["sourceNode"] = "archive-node"
			}
			_ = unstructured.SetNestedSlice(group.Object, pods, "spec", "pods")
			if err := r.Update(ctx, group); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, key); err != nil {
				t.Fatal(err)
			}
			cleanup := p.NewObject("SpotRecovery")
			if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "operation-cleanup"}, cleanup); err != nil {
				t.Fatal(err)
			}
			if readRecoverySpec(cleanup).Operation != "operation" {
				t.Fatal("replacement owner identity lost")
			}
			switch scenario {
			case "owner-uid":
				a := group.GetAnnotations()
				a[groupCleanupOwnerUID] = "other"
				group.SetAnnotations(a)
			case "fence-operation":
				_ = unstructured.SetNestedField(group.Object, "operation", "status", "verification", "sourceFence", "operation")
			case "checkpoint":
				_ = unstructured.SetNestedField(group.Object, "other", "spec", "checkpointRef", "uid")
			case "missing-handoff":
				if err := r.Get(ctx, client.ObjectKeyFromObject(op), op); err != nil {
					t.Fatal(err)
				}
				unstructured.RemoveNestedField(op.Object, "status", "partialFallback")
				if err := r.Status().Update(ctx, op); err != nil {
					t.Fatal(err)
				}
			case "source-identities":
				_ = unstructured.SetNestedSlice(group.Object, []interface{}{}, "spec", "groupRestore", "sourcePods")
			case "volumes":
				_ = unstructured.SetNestedField(group.Object, false, "spec", "volumesReady")
			}
			if err := r.Update(ctx, group); err != nil {
				t.Fatal(err)
			}
			recovery := &RecoveryReconciler{Client: r.Client, APIReader: r.Client, Clock: r.Clock}
			phase, err := recovery.recover(ctx, cleanup)
			got := p.NewObject("NodeProvision")
			getErr := r.Get(ctx, client.ObjectKeyFromObject(old), got)
			if scenario != "valid" {
				if err == nil || phase == phaseCleanupRequested || getErr != nil {
					t.Fatalf("unsafe cleanup: phase=%s err=%v old=%v", phase, err, getErr)
				}
				return
			}
			if phase != phaseCleanupRequested || !apierrors.IsNotFound(getErr) {
				t.Fatalf("cleanup blocked: phase=%s err=%v old=%v", phase, err, getErr)
			}
			if phase, err = recovery.recover(ctx, cleanup); err != nil || phase != "Completed" {
				t.Fatalf("completion: %s %v", phase, err)
			}
		})
	}
}

func TestRecordedEmergencyFallbackAcceptsOnlyOwnedFence(t *testing.T) {
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	for _, reason := range []string{"target_source_pod_replaced", "survivor_pod_replaced", "failed_partial_all_originals_replaced"} {
		for _, scenario := range []string{"valid", "wrong-operation", "wrong-instance", "wrong-source", "missing-handoff"} {
			t.Run(reason+"/"+scenario, func(t *testing.T) {
				op, _, _, old, _, _, _, _ := emergencyFallbackFixtures(t, now)
				spec, err := readReplacementSpec(op)
				if err != nil {
					t.Fatal(err)
				}
				fallback := map[string]interface{}{"phase": partialFallbackHandoffRecorded, "reason": reason}
				fence := map[string]interface{}{"operationUID": string(old.GetUID()), "instanceID": "i-1"}
				switch scenario {
				case "wrong-operation":
					fence["operationUID"] = spec.Operation
				case "wrong-instance":
					fence["instanceID"] = "i-other"
				case "wrong-source":
					old.SetUID(types.UID("other"))
				case "missing-handoff":
					delete(fallback, "phase")
				}
				_ = unstructured.SetNestedMap(old.Object, fence, "spec", "fence")
				_ = unstructured.SetNestedMap(old.Object, fence, "status", "fence")
				err = verifyFallbackOldNode(old, spec, fallback)
				if (err == nil) != (scenario == "valid") {
					t.Fatalf("fence acceptance err=%v", err)
				}
				if scenario == "valid" {
					if err := verifyReplacementOldNode(old, spec); err == nil || !strings.Contains(err.Error(), "fence") {
						t.Fatal("partial source fence guard weakened")
					}
					if stringField(old.Object, "spec", "fence", "instanceID") != "i-1" {
						t.Fatal("validator mutated source fence")
					}
				}
			})
		}
	}
}
