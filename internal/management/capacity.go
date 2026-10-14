package management

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errPartialReplacementNotReady = errors.New("partial replacement prerequisites are not ready; group fallback is disabled")

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

type capacityReplacementMismatch struct {
	Node            *unstructured.Unstructured
	ExistingMarket  string
	DesiredMarket   string
	OperationName   string
	ReplacementName string
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
	if boolField(policyObj.Object, "spec", "replacement", "enabled") {
		for i := int64(0); i < decision.DesiredWorkers; i++ {
			slot := trainingpolicy.NewNodeProvision(input, i, desiredMarketForOrdinal(i, decision))
			old := owned[slot.GetName()]
			desiredMarket := desiredMarketForOrdinal(i, decision)
			if old == nil || old.GetUID() == "" || !old.GetDeletionTimestamp().IsZero() || stringField(old.Object, "spec", "marketType") == desiredMarket {
				continue
			}
			name := replacementNodeProvisionName(old.GetName(), string(old.GetUID()))
			if reusableCapacityReplacement(owned[name], old, input, desiredMarket) {
				// Preserve the source slot while reusing its UID-bound temporary capacity.
				delete(owned, name)
			}
		}
	}
	if int64(len(owned)) > input.TargetWorkers {
		result.Blocked = true
		result.Reason = "capacity_overshoot"
		result.Overshoot = true
		result.Message = fmt.Sprintf("owned NodeProvision inventory has %d workers, targetWorkers is %d; no automatic deletion is performed", result.OwnedWorkers, input.TargetWorkers)
		return result, nil
	}

	missingGenerated := int64(0)
	mismatches := []capacityReplacementMismatch{}
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
			mismatches = append(mismatches, capacityReplacementMismatch{
				Node:            existing,
				ExistingMarket:  existingMarket,
				DesiredMarket:   desiredMarket,
				OperationName:   replacementOperationName(existing.GetName(), oldUID),
				ReplacementName: replacementNodeProvisionName(existing.GetName(), oldUID),
			})
		}
	}
	if projected := int64(len(owned)) + missingGenerated; projected > input.TargetWorkers {
		result.Blocked = true
		result.Reason = "capacity_inventory_full"
		result.Overshoot = true
		result.Message = fmt.Sprintf("owned NodeProvision inventory has %d workers and %d generated slots are missing; projected capacity %d exceeds fixed targetWorkers %d", result.OwnedWorkers, missingGenerated, projected, input.TargetWorkers)
		return result, nil
	}
	if len(mismatches) > 0 {
		result.Blocked = true
		result.Reason = "replacement_required"
		result.ReplacementRequired = true
		result.OperationName = mismatches[0].OperationName
		result.ReplacementName = mismatches[0].ReplacementName
		if !boolField(policyObj.Object, "spec", "replacement", "enabled") {
			first := mismatches[0]
			result.Message = fmt.Sprintf("existing NodeProvision %s/%s uid=%s has immutable marketType=%s; desired=%s; replacement orchestration requires spec.replacement.enabled=true and an explicit UID-bound SpotReplacement", first.Node.GetNamespace(), first.Node.GetName(), string(first.Node.GetUID()), first.ExistingMarket, first.DesiredMarket)
			return result, nil
		}
		preparedCount := 0
		if policyObj.GetAnnotations()["training.dcnlab.com/planned-partial"] != "disabled" {
			for _, mismatch := range mismatches {
				prepared, err := r.ensurePreparedReplacementNodeProvision(ctx, policyObj, input, mismatch.Node, mismatch.OperationName, mismatch.ReplacementName, mismatch.DesiredMarket)
				if err != nil {
					result.Reason = "replacement_unsupported"
					result.OperationName = mismatch.OperationName
					result.ReplacementName = mismatch.ReplacementName
					result.Message = err.Error()
					return result, nil
				}
				if prepared {
					preparedCount++
				}
			}
		}
		active, err := (&CheckpointReconciler{Client: r.Client}).activeSpotReplacement(ctx, input)
		if err != nil {
			return result, err
		}
		if active != nil {
			result.OperationName = active.GetName()
			result.ReplacementName = stringField(active.Object, "spec", "replacementNodeProvisionRef", "name")
			result.Message = fmt.Sprintf("prepared replacement NodeProvision capacity for %d desired market mismatch(es); SpotReplacement %s remains the serial operation owner", len(mismatches), active.GetName())
			return result, nil
		}
		created, err := r.ensureAutomaticSpotReplacement(ctx, policyObj, input, mismatches[0].Node, mismatches[0].OperationName, mismatches[0].ReplacementName, mismatches[0].DesiredMarket, "")
		if err != nil {
			result.Reason = "replacement_unsupported"
			if errors.Is(err, errPartialReplacementNotReady) {
				result.Reason = "partial_replacement_waiting"
			}
			result.Message = err.Error()
			return result, nil
		}
		if created {
			result.Message = fmt.Sprintf("prepared %d replacement NodeProvision(s) and created serial SpotReplacement %s for %d desired market mismatch(es)", preparedCount, result.OperationName, len(mismatches))
		} else {
			result.Message = fmt.Sprintf("prepared replacement NodeProvision capacity for %d desired market mismatch(es); serial SpotReplacement %s exists", len(mismatches), result.OperationName)
		}
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
	// Missing partial evidence must not widen deletion to the whole world.
	// Group replacement requires an explicit opt-out of planned partial.
	if policyObj.GetAnnotations()["training.dcnlab.com/planned-partial"] != "disabled" {
		selected, created, err := r.ensurePlannedPartialReplacement(ctx, policyObj, input, oldNP, operationName, replacementName, desiredMarketType, emergencyEventID)
		if selected || err != nil {
			return created, err
		}
		return false, errPartialReplacementNotReady
	}
	return r.ensureGroupReplacement(ctx, policyObj, input, oldNP, operationName, replacementName, desiredMarketType, emergencyEventID)
}

func (r *PolicyReconciler) ensurePreparedReplacementNodeProvision(ctx context.Context, policyObj *unstructured.Unstructured, input trainingpolicy.PolicyInput, oldNP *unstructured.Unstructured, operationName, replacementName, desiredMarketType string) (bool, error) {
	if oldNP.GetUID() == "" {
		return false, fmt.Errorf("replacement requires old NodeProvision UID evidence")
	}
	spec := replacementSpec{
		Operation:                    operationName,
		PolicyName:                   input.PolicyName,
		PolicyUID:                    string(input.PolicyUID),
		OldNodeProvisionName:         oldNP.GetName(),
		OldNodeProvisionUID:          string(oldNP.GetUID()),
		ReplacementNodeProvisionName: replacementName,
		DesiredMarketType:            desiredMarketType,
	}
	existing := trainingpolicy.NewObject("NodeProvision")
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: input.Namespace, Name: replacementName}, existing)
	if err == nil {
		if err := replacementNodeProvisionIdentityError(existing, input.Namespace, spec); err != nil {
			return false, err
		}
		return false, r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, existing, input.Capacity.AWSCluster))
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	if policyObj.GetAnnotations()[groupIntentAnnotation] != "" || input.SourceCluster != "aws" || input.Capacity.AWSCluster != "aws" {
		return false, errPartialReplacementNotReady
	}
	if active, err := activeGroupRestore(ctx, r.reader(), input); err != nil {
		return false, err
	} else if active != nil {
		return false, errPartialReplacementNotReady
	}
	if err := verifyReplacementOldNode(oldNP, spec); err != nil {
		return false, err
	}
	if stringField(oldNP.Object, "status", "phase") != "Ready" {
		return false, fmt.Errorf("replacement source NodeProvision is not Ready")
	}
	runtime := trainingpolicy.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: input.Namespace, Name: input.RuntimeRefName}, runtime); err != nil {
		return false, err
	}
	health := trainingpolicy.ReadRuntimeStatus(runtime, input.SourceCluster)
	if !trainingpolicy.RuntimeReadyForCheckpoint(input, health, r.now()) {
		return false, errPartialReplacementNotReady
	}
	if _, err := r.groupSourcePods(ctx, input); err != nil {
		return false, err
	}
	if _, _, err := r.groupCheckpointVolume(ctx, input); err != nil {
		return false, err
	}
	desired, err := buildReplacementNodeProvision(input.Namespace, oldNP, spec)
	if err != nil {
		return false, err
	}
	if err := r.Create(ctx, desired); err != nil {
		return false, err
	}
	if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, desired, input.Capacity.AWSCluster)); err != nil {
		return true, err
	}
	return true, nil
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
	groups := &unstructured.UnstructuredList{}
	groups.SetGroupVersionKind(newRestoreRequest().GroupVersionKind().GroupVersion().WithKind("RestoreRequestList"))
	if err := r.reader().List(ctx, groups, client.InNamespace(input.Namespace), client.MatchingLabels{trainingpolicy.LabelPolicyUID: string(input.PolicyUID), trainingpolicy.LabelRole: groupRestoreRole}); err != nil {
		return nil, err
	}
	for i := range groups.Items {
		item := &groups.Items[i]
		if validateGroupVerified(item) != nil {
			continue
		}
		a := item.GetAnnotations()
		oldName, oldUID, newName, newUID := a[groupOldName], a[groupOldUID], a[groupNewName], a[groupNewUID]
		if oldName == "" || oldUID == "" || newName == "" || newUID == "" {
			continue
		}
		if existing := edges[oldName]; existing != "" && (existing != newName || oldUIDs[oldName] != oldUID || newUIDs[oldName] != newUID) {
			return nil, fmt.Errorf("conflicting group replacement successor")
		}
		edges[oldName] = newName
		oldUIDs[oldName] = oldUID
		newUIDs[oldName] = newUID
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
