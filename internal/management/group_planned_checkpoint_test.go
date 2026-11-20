package management

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func healthyPlannedRuntime(t *testing.T, r *PolicyReconciler, input p.PolicyInput) {
	t.Helper()
	rt := p.NewObject("TrainingRuntime")
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: input.Namespace, Name: input.RuntimeRefName}, rt); err != nil {
		t.Fatal(err)
	}
	report := nestedClusterStatuses(rt.Object)[0]["status"].(map[string]interface{})
	stamp := r.now().UTC().Format(time.RFC3339)
	pods := report["sourcePods"].([]interface{})
	for _, raw := range pods {
		raw.(map[string]interface{})["observedAt"] = stamp
	}
	report["phase"], report["workloadUID"], report["memberWorkloadUID"] = "Running", string(input.WorkloadRef.UID), "member-workload"
	report["readyRanks"], report["worldSize"], report["observedAt"], report["pods"] = input.TargetWorkers, input.TargetWorkers, stamp, pods
	_ = unstructured.SetNestedSlice(rt.Object, []interface{}{map[string]interface{}{"clusterName": input.SourceCluster, "status": report}}, "status", "clusters")
	if err := r.Update(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
}

// Existing lifecycle tests simulate actual checkpoint completion explicitly;
// production code never manufactures or rewrites this status.
func completePlannedBatchCheckpoint(t *testing.T, r *PolicyReconciler, intent groupOperation) {
	t.Helper()
	ctx := context.Background()
	policy := p.NewObject("NodeRequest")
	if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "policy"}, policy); err != nil {
		t.Fatal(err)
	}
	input := p.ReadPolicyInput(policy)
	healthyPlannedRuntime(t, r, input)
	source, err := r.groupSourcePods(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	pairs := []groupReplacementPair{}
	for _, target := range intent.Batch {
		np := p.NewObject("NodeProvision")
		if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: target.Replacement}, np); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, groupReplacementPair{target.OldName, target.OldUID, target.Replacement, string(np.GetUID())})
	}
	if round, err := r.ensurePlannedGroupCheckpoint(ctx, policy, input, intent.Operation, source, pairs); err != nil || round != nil {
		t.Fatalf("initial fresh checkpoint: round=%v err=%v", round, err)
	}
	finishPlannedCheckpoint(t, r, input, source)
}

func finishPlannedCheckpoint(t *testing.T, r *PolicyReconciler, input p.PolicyInput, source []interface{}) *unstructured.Unstructured {
	t.Helper()
	list := p.NewList("FluidCRMigration")
	if err := r.List(context.Background(), list, client.InNamespace(input.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		cp := &list.Items[i]
		if cp.GetAnnotations()[plannedCheckpointReceipt] == "" {
			continue
		}
		cp.SetUID(types.UID("fresh-checkpoint"))
		cp.SetGeneration(1)
		template, _ := groupCheckpointFixture()
		report := groupReport(template)
		base := report["pods"].([]interface{})[0].(map[string]interface{})
		pods := []interface{}{}
		for _, raw := range source {
			s := raw.(map[string]interface{})
			pod := deepCopyMap(base)
			pod["podName"], pod["podUID"], pod["nodeName"] = s["podName"], s["podUID"], s["nodeName"]
			pods = append(pods, pod)
		}
		report["pods"], report["completionTime"] = pods, r.now().UTC().Format(time.RFC3339)
		cp.Object["status"] = map[string]interface{}{"clusters": []interface{}{report}}
		if err := r.Update(context.Background(), cp); err != nil {
			t.Fatal(err)
		}
		if err := r.Update(context.Background(), cp); err != nil {
			t.Fatal(err)
		}
		return cp
	}
	t.Fatal("fresh checkpoint was not requested")
	return nil
}

func TestPlannedReplacementWaitsForFreshDurableRoundAndRejectsChangedSources(t *testing.T) {
	for _, mode := range []string{"success", "failed", "stale-archive", "source-changed", "target-changed", "incomplete-export"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, policy, input, intent := batchFixture(t, false)
			if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
				t.Fatal(err)
			}
			// Mark targets Ready without the legacy-test convenience completion.
			for i, target := range intent.Batch {
				np := p.NewObject("NodeProvision")
				if err := r.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: target.Replacement}, np); err != nil {
					t.Fatal(err)
				}
				np.SetUID(types.UID(fmt.Sprintf("new-%d", i)))
				np.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": fmt.Sprintf("new-node-%d", i)}
				if err := r.Update(ctx, np); err != nil {
					t.Fatal(err)
				}
			}
			healthyPlannedRuntime(t, r, input)
			r.GroupStage = groupStageRequest
			if created, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil || created {
				t.Fatalf("old round accepted: created=%v err=%v", created, err)
			}
			if err := r.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: intent.Operation + "-group-restore"}, newRestoreRequest()); !apierrors.IsNotFound(err) {
				t.Fatal("restore fenced sources before fresh checkpoint completed")
			}
			source, err := r.groupSourcePods(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			cp := finishPlannedCheckpoint(t, r, input, source)
			switch mode {
			case "failed":
				groupReport(cp)["phase"] = "Failed"
			case "stale-archive":
				groupReport(cp)["pods"].([]interface{})[0].(map[string]interface{})["podUID"] = "old-archived-uid"
			case "source-changed":
				a := cp.GetAnnotations()
				a[plannedCheckpointReceipt] = "changed"
				cp.SetAnnotations(a)
			case "target-changed":
				np := p.NewObject("NodeProvision")
				_ = r.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: intent.Batch[1].Replacement}, np)
				np.SetUID("recreated")
				if err := r.Update(ctx, np); err != nil {
					t.Fatal(err)
				}
			case "incomplete-export":
				delete(groupFile(cp), "exportedAt")
			}
			if err := r.Update(ctx, cp); err != nil {
				t.Fatal(err)
			}
			if err := r.Update(ctx, cp); err != nil {
				t.Fatal(err)
			}
			created, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent)
			if mode == "success" {
				if err != nil || !created {
					t.Fatalf("created=%v err=%v", created, err)
				}
				req := batchRequest(t, r, intent)
				if stringField(req.Object, "spec", "checkpointRef", "uid") != "fresh-checkpoint" {
					t.Fatal("restore used old checkpoint")
				}
			} else if err == nil || created {
				t.Fatalf("unsafe %s checkpoint accepted", mode)
			}
		})
	}
}

func TestSinglePlannedWorkerReplacementAlsoPinsFreshFullWorldCheckpoint(t *testing.T) {
	ctx := context.Background()
	r, policy, input, intent := batchFixture(t, false)
	old := p.NewObject("NodeProvision")
	if err := r.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: intent.OldName}, old); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ensureGroupReplacement(ctx, policy, input, old, intent.Operation, intent.Replacement, intent.Market, ""); err != nil && !strings.Contains(err.Error(), "waiting for group replacement NodeProvision") {
		t.Fatal(err)
	}
	np := p.NewObject("NodeProvision")
	if err := r.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: intent.Replacement}, np); err != nil {
		t.Fatal(err)
	}
	np.SetUID("new-single")
	np.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": "new-node-single"}
	if err := r.Update(ctx, np); err != nil {
		t.Fatal(err)
	}
	healthyPlannedRuntime(t, r, input)
	r.GroupStage = groupStageRequest
	if created, err := r.ensureGroupReplacement(ctx, policy, input, old, intent.Operation, intent.Replacement, intent.Market, ""); err != nil || created {
		t.Fatalf("old archive selected: %v %v", created, err)
	}
	source, err := r.groupSourcePods(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	finishPlannedCheckpoint(t, r, input, source)
	if created, err := r.ensureGroupReplacement(ctx, policy, input, old, intent.Operation, intent.Replacement, intent.Market, ""); err != nil || !created {
		t.Fatalf("fresh archive rejected: %v %v", created, err)
	}
	req := batchRequest(t, r, intent)
	if stringField(req.Object, "spec", "checkpointRef", "uid") != "fresh-checkpoint" {
		t.Fatal("single replacement used stale archive")
	}
}
