package management

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const plannedCheckpointReceipt = "training.dcnlab.com/planned-checkpoint-receipt"

// Called only for healthy, planned replacement, after every target is Ready.
// Emergency recovery deliberately retains its last-durable-checkpoint path.
func (r *PolicyReconciler) ensurePlannedGroupCheckpoint(ctx context.Context, policy *unstructured.Unstructured, input p.PolicyInput, operation string, source []interface{}, pairs []groupReplacementPair) (*groupCheckpoint, error) {
	if operation == "" || len(pairs) == 0 || !input.Checkpoint.Resume || input.Suspended || !policy.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("planned checkpoint requires an active operation")
	}
	receipt, err := json.Marshal(struct {
		PolicyUID  string
		Generation int64
		WorldUID   string
		Operation  string
		Source     []interface{}
		Targets    []groupReplacementPair
	}{string(input.PolicyUID), policy.GetGeneration(), string(input.WorkloadRef.UID), operation, source, pairs})
	if err != nil {
		return nil, err
	}
	name := dnsLabelPrefix(input.PolicyName, 32) + "-planned-" + eventHash(string(input.PolicyUID), operation)
	cp := p.NewObject("FluidCRMigration")
	err = r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: name}, cp)
	if apierrors.IsNotFound(err) {
		coordinator := &CheckpointReconciler{Client: r.Client, Clock: r.Clock}
		runtime, e := coordinator.runtimeSnapshot(ctx, input)
		if e != nil {
			return nil, e
		}
		if !p.RuntimeReadyForCheckpoint(input, runtime, r.now()) {
			return nil, fmt.Errorf("planned checkpoint waits for a healthy current training world")
		}
		for _, raw := range source {
			s := raw.(map[string]interface{})
			matched := false
			for _, pod := range runtime.Pods {
				if pod.Rank == intField(s, "rank") && pod.Name == stringField(s, "podName") && pod.UID == stringField(s, "podUID") {
					matched = true
				}
			}
			if !matched {
				return nil, fmt.Errorf("planned checkpoint runtime/source Pod identity mismatch")
			}
		}
		cp = p.NewFluidCRMigration(input, runtime, r.now(), 1)
		cp.SetName(name)
		a := cp.GetAnnotations()
		a["training.dcnlab.com/checkpoint-id"] = name
		a["training.dcnlab.com/recovery-operation"] = operation
		a[plannedCheckpointReceipt] = string(receipt)
		cp.SetAnnotations(a)
		// Preserve existing resume behavior so a failed restore request cannot
		// leave a healthy source training world parked indefinitely.
		if err := r.Create(ctx, cp); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if cp.GetAnnotations()[plannedCheckpointReceipt] != string(receipt) || cp.GetAnnotations()["training.dcnlab.com/checkpoint-id"] != name || cp.GetAnnotations()["training.dcnlab.com/recovery-operation"] != operation || cp.GetLabels()[p.LabelRole] != "checkpoint" || cp.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) || !cp.GetDeletionTimestamp().IsZero() || stringField(cp.Object, "spec", "workloadRef", "uid") != string(input.WorkloadRef.UID) || !boolField(cp.Object, "spec", "resume") || isCheckpointSchedule(cp) {
		return nil, fmt.Errorf("planned checkpoint identity, source, target or resume intent changed")
	}
	if err := r.createIfMissing(ctx, p.NewPropagationPolicyFor(input, cp, input.SourceCluster)); err != nil {
		return nil, err
	}
	report, ok := sourceClusterStatus(cp, input.SourceCluster)
	if !ok || intField(report, "observedGeneration") != cp.GetGeneration() {
		return nil, nil
	}
	if stringField(report, "phase") == "Failed" {
		return nil, fmt.Errorf("planned checkpoint failed; old checkpoint fallback is forbidden")
	}
	if stringField(report, "phase") != "Completed" {
		return nil, nil
	}
	round, err := readGroupCheckpoint(cp, input)
	if err != nil {
		return nil, err
	}
	started, err := time.Parse(time.RFC3339, cp.GetAnnotations()["training.dcnlab.com/started-at"])
	if err != nil || round.CompletedAt.Before(started) {
		return nil, fmt.Errorf("planned checkpoint completion predates target readiness")
	}
	for i, raw := range round.Pods {
		archived := raw.(map[string]interface{})
		current := source[i].(map[string]interface{})
		if intField(archived, "rank") != intField(current, "rank") || stringField(archived, "sourcePod") != stringField(current, "podName") || stringField(archived, "sourcePodUID") != stringField(current, "podUID") || stringField(archived, "sourceNode") != stringField(current, "nodeName") {
			return nil, fmt.Errorf("planned checkpoint archived a different source world")
		}
	}
	return round, nil
}
