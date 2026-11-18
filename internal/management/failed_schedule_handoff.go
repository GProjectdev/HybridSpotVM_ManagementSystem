package management

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const verifiedScheduledRetirements = "training.dcnlab.com/verified-scheduled-retirements"

type scheduledRetirement struct {
	PolicyUID             string            `json:"policyUID"`
	SourceWorldUID        string            `json:"sourceWorldUID"`
	ChildUID              string            `json:"childUID"`
	ChildGeneration       int64             `json:"childGeneration"`
	CheckpointID          string            `json:"checkpointID"`
	PodUIDs               map[string]string `json:"podUIDs"`
	RestoreRequestUID     string            `json:"restoreRequestUID"`
	OperationUID          string            `json:"operationUID"`
	SourceFenceEvidenceID string            `json:"sourceFenceEvidenceID"`
	VerifiedAt            string            `json:"verifiedAt"`
}

func fullRankPodUIDs(raw []interface{}, size int64) (map[string]string, error) {
	if size < 1 || int64(len(raw)) != size {
		return nil, fmt.Errorf("complete source rank identities required")
	}
	result := map[string]string{}
	seen := map[string]bool{}
	for _, value := range raw {
		pod, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid source pod identity")
		}
		rank, ok := pod["rank"].(int64)
		uid := stringField(pod, "podUID")
		key := strconv.FormatInt(rank, 10)
		if !ok || rank < 0 || rank >= size || uid == "" || seen[uid] || result[key] != "" {
			return nil, fmt.Errorf("duplicate or invalid source rank UID")
		}
		result[key] = uid
		seen[uid] = true
	}
	return result, nil
}

// Only a fresh Verified full-group result with real source-fence evidence may
// retire a failed scheduler blocker. The Failed child and its bytes are retained.
func (r *GroupRecoveryReconciler) retireFailedScheduledChildren(ctx context.Context, policy, req *unstructured.Unstructured) error {
	if err := validateGroupVerified(req); err != nil {
		return err
	}
	world := stringField(req.Object, "spec", "groupRestore", "sourceWorldUID")
	if policy.GetUID() == "" || world == "" || world != stringField(policy.Object, "spec", "workloadRef", "uid") || req.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) {
		return fmt.Errorf("verified retirement policy/world mismatch")
	}
	list := p.NewList("FluidCRMigration")
	if err := r.Reader.List(ctx, list, client.InNamespace(policy.GetNamespace()), client.MatchingLabels{p.LabelPolicyUID: string(policy.GetUID()), p.LabelRole: checkpointScheduleRole}); err != nil {
		return err
	}
	cluster := stringField(req.Object, "spec", "sourceCluster")
	for i := range list.Items {
		schedule := &list.Items[i]
		if !schedule.GetDeletionTimestamp().IsZero() || schedule.GetUID() == "" || stringField(schedule.Object, "spec", "workloadRef", "uid") != world {
			continue
		}
		report, ok := sourceClusterStatus(schedule, cluster)
		if !ok || intField(report, "observedGeneration") != schedule.GetGeneration() {
			continue
		}
		run, _, _ := unstructured.NestedMap(report, "currentRun")
		if stringField(run, "phase") != "Failed" || stringField(run, "uid") == "" || stringField(run, "checkpointID") == "" {
			continue
		}
		status, _, _ := unstructured.NestedMap(run, "result", "status")
		spec, _, _ := unstructured.NestedMap(run, "result", "spec")
		if stringField(status, "phase") != "Failed" || intField(status, "observedGeneration") < 1 || stringField(spec, "workloadRef", "uid") != world || spec["partialCheckpoint"] != nil || spec["schedule"] != nil {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, stringField(status, "completionTime")); err != nil {
			continue
		}
		size := intField(req.Object, "spec", "groupRestore", "worldSize")
		source, _, _ := unstructured.NestedSlice(req.Object, "spec", "groupRestore", "sourcePods")
		pods, err := fullRankPodUIDs(source, size)
		if err != nil {
			return err
		}
		failed, _, _ := unstructured.NestedSlice(status, "pods")
		failedPods, err := fullRankPodUIDs(failed, size)
		if err != nil || !reflect.DeepEqual(pods, failedPods) {
			continue
		}
		receipt := scheduledRetirement{PolicyUID: string(policy.GetUID()), SourceWorldUID: world, ChildUID: stringField(run, "uid"), ChildGeneration: intField(status, "observedGeneration"), CheckpointID: stringField(run, "checkpointID"), PodUIDs: pods, RestoreRequestUID: string(req.GetUID()), OperationUID: stringField(req.Object, "spec", "groupRestore", "operationUID"), SourceFenceEvidenceID: stringField(req.Object, "status", "verification", "sourceFence", "evidenceID"), VerifiedAt: stringField(req.Object, "status", "verification", "verifiedAt")}
		records := map[string]scheduledRetirement{}
		a := schedule.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		if text := a[verifiedScheduledRetirements]; text != "" {
			if err := json.Unmarshal([]byte(text), &records); err != nil {
				return fmt.Errorf("invalid verified schedule retirement receipt")
			}
		}
		if prior, ok := records[receipt.ChildUID]; ok {
			prior.VerifiedAt = receipt.VerifiedAt
			prior.SourceFenceEvidenceID = receipt.SourceFenceEvidenceID
			if !reflect.DeepEqual(prior, receipt) {
				return fmt.Errorf("failed child already retired by a different verified operation")
			}
			continue
		}
		records[receipt.ChildUID] = receipt
		data, err := json.Marshal(records)
		if err != nil {
			return err
		}
		before := schedule.DeepCopy()
		a[verifiedScheduledRetirements] = string(data)
		schedule.SetAnnotations(a)
		if err := r.Patch(ctx, schedule, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}
