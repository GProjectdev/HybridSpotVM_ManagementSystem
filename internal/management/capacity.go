package management

import (
	"context"
	"fmt"
	"strings"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
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

	retired, err := r.retiredGeneratedSlots(ctx, input)
	if err != nil {
		return result, err
	}
	if len(retired) > 0 {
		result.Blocked = true
		result.Reason = "retired_generated_slot"
		result.Message = fmt.Sprintf("current policy has retired generated NodeProvision slot %q; refusing to recreate generated originals", firstMapKey(retired))
		return result, nil
	}

	owned, err := r.ownedWorkerNodeProvisions(ctx, input)
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
			result.Blocked = true
			result.Reason = "replacement_required"
			result.ReplacementRequired = true
			result.Message = fmt.Sprintf("existing NodeProvision %s/%s has immutable marketType=%s; desired=%s requires explicit replacement, not ratio reprovisioning", existing.GetNamespace(), existing.GetName(), existingMarket, desiredMarket)
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

func (r *PolicyReconciler) ownedWorkerNodeProvisions(ctx context.Context, input trainingpolicy.PolicyInput) (map[string]*unstructured.Unstructured, error) {
	list := trainingpolicy.NewList("NodeProvision")
	labels := client.MatchingLabels{
		trainingpolicy.LabelPolicyUID: string(input.PolicyUID),
		trainingpolicy.LabelRole:      "worker",
	}
	if err := r.reader().List(ctx, list, client.InNamespace(input.Namespace), labels); err != nil {
		return nil, fmt.Errorf("list owned NodeProvisions: %w", err)
	}
	owned := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		item := list.Items[i].DeepCopy()
		owned[item.GetName()] = item
	}
	return owned, nil
}

func (r *PolicyReconciler) retiredGeneratedSlots(ctx context.Context, input trainingpolicy.PolicyInput) (map[string]bool, error) {
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
		retired[name] = true
	}
	return retired, nil
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
}

func isGeneratedWorkerName(input trainingpolicy.PolicyInput, name string) bool {
	return strings.HasPrefix(name, fmt.Sprintf("%s-worker-", input.PolicyName))
}

func firstMapKey(values map[string]bool) string {
	for key := range values {
		return key
	}
	return ""
}
