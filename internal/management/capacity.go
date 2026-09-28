package management

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type capacityDecision struct {
	Blocked             bool
	Reason              string
	Message             string
	Overshoot           bool
	ReplacementRequired bool
	OwnedWorkers        int64
	TargetWorkers       int64
	HistoricalWorkers   int64
	OperationName       string
	ReplacementName     string
}

func (r *PolicyReconciler) checkCapacityLifecycle(ctx context.Context, policyObj *unstructured.Unstructured, input trainingpolicy.PolicyInput, decision trainingpolicy.Decision) (capacityDecision, error) {
	result := capacityDecision{OwnedWorkers: 0, TargetWorkers: input.TargetWorkers}

	if previous, ok, _ := unstructured.NestedInt64(policyObj.Object, "status", trainingpolicy.StatusPolicyPath, "desiredWorkers"); ok && previous > 0 && previous != input.TargetWorkers {
		result.Blocked = true
		result.Reason = "fixed_target_changed"
		result.Message = fmt.Sprintf("spec.targetWorkers changed from historical fixed value %d to %d; create a new TrainingPolicy for capacity shape changes", previous, input.TargetWorkers)
		result.HistoricalWorkers = previous
		return result, nil
	}

	successors, err := r.completedReplacementSuccessors(ctx, input)
	if err != nil {
		return result, err
	}
	retired, err := r.retiredGeneratedSlots(ctx, input, successors)
	if err != nil {
		return result, err
	}
	if len(retired) > 0 {
		result.Blocked = true
		result.Reason = "retired_generated_slot"
		result.Message = fmt.Sprintf("current policy has retired generated NodeProvision slot %q; refusing to recreate generated originals", firstMapKey(retired))
		return result, nil
	}

	owned, err := r.ownedWorkerNodeProvisions(ctx, input, successors)
	if err != nil {
		return result, err
	}
	result.OwnedWorkers = int64(len(owned))
	if result.OwnedWorkers > input.TargetWorkers {
		result.Blocked = true
		result.Reason = "capacity_overshoot"
		result.Overshoot = true
		result.Message = fmt.Sprintf("owned NodeProvision inventory has %d workers, targetWorkers is %d; no automatic deletion is performed", result.OwnedWorkers, input.TargetWorkers)
		return result, nil
	}

	missingGenerated := int64(0)
	for i := int64(0); i < decision.DesiredWorkers; i++ {
		desired := trainingpolicy.NewNodeProvision(input, i, desiredMarketForOrdinal(i, decision))
		existing, exists := owned[desired.GetName()]
		if !exists {
			missingGenerated++
			continue
		}
		existingMarket, _, _ := unstructured.NestedString(existing.Object, "spec", "marketType")
		desiredMarket, _, _ := unstructured.NestedString(desired.Object, "spec", "marketType")
		if existingMarket != "" && desiredMarket != "" && existingMarket != desiredMarket {
			oldUID := string(existing.GetUID())
			result.Blocked = true
			result.Reason = "replacement_required"
			result.ReplacementRequired = true
			result.OperationName = replacementOperationName(existing.GetName(), oldUID)
			result.ReplacementName = replacementNodeProvisionName(existing.GetName(), oldUID)
			if boolField(policyObj.Object, "spec", "replacement", "enabled") {
				created, err := r.ensureAutomaticSpotReplacement(ctx, policyObj, input, existing, result.OperationName, result.ReplacementName, desiredMarket, "")
				if err != nil {
					result.Reason = "replacement_unsupported"
					result.Message = err.Error()
					return result, nil
				}
				if created {
					result.Message = fmt.Sprintf("created UID-bound SpotReplacement %s for NodeProvision %s/%s uid=%s desiredMarketType=%s", result.OperationName, existing.GetNamespace(), existing.GetName(), oldUID, desiredMarket)
				} else {
					result.Message = fmt.Sprintf("UID-bound SpotReplacement %s exists for NodeProvision %s/%s uid=%s desiredMarketType=%s", result.OperationName, existing.GetNamespace(), existing.GetName(), oldUID, desiredMarket)
				}
			} else {
				result.Message = fmt.Sprintf("existing NodeProvision %s/%s uid=%s has immutable marketType=%s; desired=%s; replacement orchestration requires spec.replacement.enabled=true and an explicit UID-bound SpotReplacement", existing.GetNamespace(), existing.GetName(), oldUID, existingMarket, desiredMarket)
			}
			return result, nil
		}
	}
	if projected := result.OwnedWorkers + missingGenerated; projected > input.TargetWorkers {
		result.Blocked = true
		result.Reason = "capacity_inventory_full"
		result.Overshoot = true
		result.Message = fmt.Sprintf("owned NodeProvision inventory has %d workers and %d generated slots are missing; projected capacity %d exceeds fixed targetWorkers %d", result.OwnedWorkers, missingGenerated, projected, input.TargetWorkers)
		return result, nil
	}

	return result, nil
}

func (r *PolicyReconciler) ensureAutomaticSpotReplacement(ctx context.Context, policyObj *unstructured.Unstructured, input trainingpolicy.PolicyInput, oldNP *unstructured.Unstructured, operationName, replacementName, desiredMarketType, emergencyEventID string) (bool, error) {
	if oldNP.GetUID() == "" {
		return false, fmt.Errorf("replacement requires old NodeProvision UID evidence")
	}
	oldMarket := stringField(oldNP.Object, "spec", "marketType")
	if !validReplacementMarket(oldMarket) || !validReplacementMarket(desiredMarketType) || oldMarket == desiredMarketType {
		return false, fmt.Errorf("replacement requires old and desired marketType to differ and be Spot/OnDemand")
	}
	pods, survivors, err := r.replacementRuntimeBaselines(ctx, input, oldNP)
	if err != nil {
		return false, err
	}
	targetRanks := make([]interface{}, 0, len(pods))
	for _, item := range pods {
		pod := item.(map[string]interface{})
		rank := intField(pod, "rank")
		if rank == 0 {
			return false, fmt.Errorf("UnsupportedRankZero: partial replacement of rank 0 is not supported")
		}
		targetRanks = append(targetRanks, rank)
	}
	existing := newSpotReplacementObject()
	err = r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: operationName}, existing)
	if err == nil {
		if stringField(existing.Object, "spec", "oldNodeProvisionRef", "uid") != string(oldNP.GetUID()) || stringField(existing.Object, "spec", "policyRef", "uid") != string(input.PolicyUID) || stringField(existing.Object, "spec", "desiredMarketType") != desiredMarketType {
			return false, fmt.Errorf("existing SpotReplacement %s collision does not match old NodeProvision/policy UID", operationName)
		}
		return false, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get SpotReplacement %s: %w", operationName, err)
	}
	op := newSpotReplacementObject()
	op.SetNamespace(input.Namespace)
	op.SetName(operationName)
	op.SetLabels(map[string]string{
		trainingpolicy.LabelManagedBy: "hybridspotvm-system",
		trainingpolicy.LabelPolicy:    input.PolicyName,
		trainingpolicy.LabelPolicyUID: string(input.PolicyUID),
		trainingpolicy.LabelRole:      "replacement-operation",
	})
	if emergencyEventID != "" {
		op.SetAnnotations(map[string]string{"training.dcnlab.com/emergency-event-id": emergencyEventID})
	}
	op.Object["spec"] = map[string]interface{}{
		"operation":                   operationName,
		"policyRef":                   map[string]interface{}{"name": input.PolicyName, "uid": string(input.PolicyUID), "generation": input.Generation},
		"workloadRef":                 map[string]interface{}{"apiVersion": input.WorkloadRef.APIVersion, "kind": input.WorkloadRef.Kind, "name": input.WorkloadRef.Name, "uid": string(input.WorkloadRef.UID)},
		"sourceCluster":               input.Capacity.AWSCluster,
		"targetCluster":               input.Capacity.AWSCluster,
		"oldNodeProvisionRef":         map[string]interface{}{"name": oldNP.GetName(), "uid": string(oldNP.GetUID())},
		"replacementNodeProvisionRef": map[string]interface{}{"name": replacementName},
		"desiredMarketType":           desiredMarketType,
		"partialCheckpoint":           map[string]interface{}{"targetRanks": targetRanks},
		"pods":                        pods,
		"partialRestore":              map[string]interface{}{"preventPeriodicResume": true, "targetRanks": targetRanks, "preservedSurvivors": survivors},
	}
	if err := r.Create(ctx, op); err != nil {
		return false, fmt.Errorf("create SpotReplacement %s: %w", operationName, err)
	}
	return true, nil
}

func (r *PolicyReconciler) replacementRuntimeBaselines(ctx context.Context, input trainingpolicy.PolicyInput, oldNP *unstructured.Unstructured) ([]interface{}, []interface{}, error) {
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.RuntimeRefName}, runtimeObj); err != nil {
		return nil, nil, fmt.Errorf("replacement requires live TrainingRuntime: %w", err)
	}
	nodeName := firstString(oldNP.Object, [][]string{{"status", "nodeName"}, {"spec", "nodeName"}, {"spec", "hostname"}})
	if nodeName == "" {
		return nil, nil, fmt.Errorf("replacement requires old NodeProvision nodeName evidence")
	}
	var targets []interface{}
	var survivors []interface{}
	for _, cluster := range nestedClusterStatuses(runtimeObj.Object) {
		name, _, _ := unstructured.NestedString(cluster, "clusterName")
		if name != input.SourceCluster {
			continue
		}
		pods, _, _ := unstructured.NestedSlice(cluster, "status", "pods")
		for _, item := range pods {
			pod, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			podNode := firstString(pod, [][]string{{"nodeName"}, {"sourceNode"}})
			rank := intField(pod, "rank")
			podName := firstString(pod, [][]string{{"name"}, {"podName"}})
			podUID := firstString(pod, [][]string{{"uid"}, {"podUID"}})
			if podName == "" || podUID == "" || podNode == "" || rank < 0 {
				return nil, nil, fmt.Errorf("replacement runtime pod evidence incomplete for node %s", nodeName)
			}
			if podNode == nodeName {
				targets = append(targets, map[string]interface{}{"rank": rank, "sourcePod": podName, "sourcePodUID": podUID, "sourceNode": nodeName})
				continue
			}
			survivors = append(survivors, map[string]interface{}{"rank": rank, "podName": podName, "podUID": podUID, "nodeName": podNode})
		}
	}
	if len(targets) == 0 {
		return nil, nil, fmt.Errorf("replacement found no runtime rank on old NodeProvision node %s", nodeName)
	}
	if len(survivors) == 0 {
		return nil, nil, fmt.Errorf("replacement requires non-target survivor runtime baseline evidence")
	}
	return targets, survivors, nil
}

func (r *PolicyReconciler) ownedWorkerNodeProvisions(ctx context.Context, input trainingpolicy.PolicyInput, successors map[string]string) (map[string]*unstructured.Unstructured, error) {
	list := trainingpolicy.NewList("NodeProvision")
	labels := client.MatchingLabels{
		trainingpolicy.LabelPolicyUID: string(input.PolicyUID),
	}
	if err := r.reader().List(ctx, list, client.InNamespace(input.Namespace), labels); err != nil {
		return nil, fmt.Errorf("list owned NodeProvisions: %w", err)
	}
	reverseSuccessors := map[string]string{}
	for oldName, replacementName := range successors {
		reverseSuccessors[replacementName] = oldName
	}
	owned := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		item := list.Items[i].DeepCopy()
		role := item.GetLabels()[trainingpolicy.LabelRole]
		if oldName := reverseSuccessors[item.GetName()]; oldName != "" {
			owned[oldName] = item
			continue
		}
		if role == "worker" || role == "replacement" {
			owned[item.GetName()] = item
		}
	}
	return owned, nil
}

func (r *PolicyReconciler) retiredGeneratedSlots(ctx context.Context, input trainingpolicy.PolicyInput, successors map[string]string) (map[string]bool, error) {
	retired := map[string]bool{}
	list := trainingpolicy.NewList("SpotRecovery")
	if err := r.reader().List(ctx, list, client.InNamespace(input.Namespace)); err != nil {
		return nil, fmt.Errorf("list SpotRecovery retired slots: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		policyUID, _, _ := unstructured.NestedString(item.Object, "spec", "policyRef", "uid")
		if policyUID != string(input.PolicyUID) {
			continue
		}
		name, _, _ := unstructured.NestedString(item.Object, "spec", "oldNodeProvisionRef", "name")
		uid, _, _ := unstructured.NestedString(item.Object, "spec", "oldNodeProvisionRef", "uid")
		if name == "" || uid == "" || !isGeneratedWorkerName(input, name) {
			continue
		}
		if successors[name] != "" {
			continue
		}
		retired[name] = true
	}
	return retired, nil
}

func (r *PolicyReconciler) completedReplacementSuccessors(ctx context.Context, input trainingpolicy.PolicyInput) (map[string]string, error) {
	edges := map[string]string{}
	oldUIDs := map[string]string{}
	newUIDs := map[string]string{}
	list := trainingpolicy.NewList("SpotRecovery")
	if err := r.reader().List(ctx, list, client.InNamespace(input.Namespace)); err != nil {
		return nil, fmt.Errorf("list SpotRecovery successors: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if stringField(item.Object, "status", "phase") != "Completed" {
			continue
		}
		if stringField(item.Object, "spec", "policyRef", "uid") != string(input.PolicyUID) {
			continue
		}
		oldName := stringField(item.Object, "spec", "oldNodeProvisionRef", "name")
		oldUID := stringField(item.Object, "spec", "oldNodeProvisionRef", "uid")
		replacementName := stringField(item.Object, "spec", "replacementNodeProvisionRef", "name")
		replacementUID := stringField(item.Object, "spec", "replacementNodeProvisionRef", "uid")
		if oldName == "" || oldUID == "" || replacementName == "" || replacementUID == "" {
			continue
		}
		if existing := edges[oldName]; existing != "" && (existing != replacementName || oldUIDs[oldName] != oldUID || newUIDs[oldName] != replacementUID) {
			return nil, fmt.Errorf("multiple replacement successors recorded for %s", oldName)
		}
		edges[oldName] = replacementName
		oldUIDs[oldName] = oldUID
		newUIDs[oldName] = replacementUID
	}
	successors := map[string]string{}
	for oldName := range edges {
		if !isGeneratedWorkerName(input, oldName) {
			continue
		}
		seen := map[string]bool{oldName: true}
		leaf := oldName
		for edges[leaf] != "" {
			if next := edges[leaf]; edges[next] != "" && newUIDs[leaf] != oldUIDs[next] {
				return nil, fmt.Errorf("replacement successor UID discontinuity at %s", next)
			}
			leaf = edges[leaf]
			if seen[leaf] {
				return nil, fmt.Errorf("replacement successor cycle recorded for %s", oldName)
			}
			seen[leaf] = true
		}
		if leaf != oldName {
			successors[oldName] = leaf
		}
	}
	return successors, nil
}

func desiredMarketForOrdinal(ordinal int64, decision trainingpolicy.Decision) string {
	if ordinal < decision.OnDemandWorkers {
		return "OnDemand"
	}
	return "Spot"
}

func applyCapacityStatus(status map[string]interface{}, capacity capacityDecision) {
	if capacity.HistoricalWorkers > 0 {
		status["desiredWorkers"] = capacity.HistoricalWorkers
	}
	status["capacityOwnedWorkers"] = capacity.OwnedWorkers
	status["capacityTargetWorkers"] = capacity.TargetWorkers
	status["capacityOvershoot"] = capacity.Overshoot
	status["replacementRequired"] = capacity.ReplacementRequired
	if capacity.OperationName != "" {
		status["replacementOperation"] = capacity.OperationName
	}
	if capacity.ReplacementName != "" {
		status["replacementNodeProvision"] = capacity.ReplacementName
	}
}

func isGeneratedWorkerName(input trainingpolicy.PolicyInput, name string) bool {
	prefix := input.PolicyName + "-worker-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	ordinal, err := strconv.ParseInt(strings.TrimPrefix(name, prefix), 10, 64)
	return err == nil && ordinal >= 0 && name == fmt.Sprintf("%s%02d", prefix, ordinal)
}

func firstMapKey(values map[string]bool) string {
	for key := range values {
		return key
	}
	return ""
}
