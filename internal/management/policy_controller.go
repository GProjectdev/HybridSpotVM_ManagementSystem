package management

import (
	"context"
	"fmt"
	"reflect"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type PolicyReconciler struct {
	client.Client
	APIReader client.Reader
	Clock     func() time.Time
}

func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	policyObj := trainingpolicy.NewObject("TrainingPolicy")
	if err := r.Get(ctx, req.NamespacedName, policyObj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !policyObj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	input := trainingpolicy.ReadPolicySpec(policyObj)
	if policyObj.GetLabels()[autoLabel] == "true" {
		target, err := automaticPlacement(ctx, r.reader(), policyObj)
		ready, _, _ := unstructured.NestedBool(policyObj.Object, "status", "discovery", "ready")
		reason := ""
		if err != nil {
			reason = err.Error()
		} else if !ready {
			reason = "waiting for workload discovery"
		} else if target != input.Capacity.AWSCluster {
			reason = "target cluster does not require AWS capacity"
		}
		if reason != "" {
			status := trainingpolicy.PolicyStatus(trainingpolicy.Decision{ProvisioningBlocked: true, Reason: "automatic_placement_gate"}, r.now())
			status["message"] = reason
			return ctrl.Result{RequeueAfter: 15 * time.Second}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusPolicyPath, status)
		}
	}
	if err := validatePolicyInput(input); err != nil {
		status := trainingpolicy.PolicyStatus(trainingpolicy.Decision{ProvisioningBlocked: true, Reason: "invalid_spec"}, r.now())
		status["message"] = err.Error()
		return ctrl.Result{}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusPolicyPath, status)
	}
	runtimeSnapshot, err := r.runtimeSnapshot(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	riskSnapshot, err := r.riskSnapshot(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	decision := trainingpolicy.Decide(input, runtimeSnapshot, riskSnapshot)
	if !trainingpolicy.RiskFreshForPolicy(input, riskSnapshot, r.now()) {
		decision.ProvisioningBlocked = true
		decision.Reason = "risk_not_fresh"
		status := trainingpolicy.PolicyStatus(decision, r.now())
		if err := patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusPolicyPath, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if err := validateRiskProfileMatchesCapacity(input, riskSnapshot); err != nil {
		decision.ProvisioningBlocked = true
		decision.Reason = "risk_profile_mismatch"
		status := trainingpolicy.PolicyStatus(decision, r.now())
		status["message"] = err.Error()
		if err := patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusPolicyPath, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if err := r.validateLiveWorkloadUID(ctx, input); err != nil {
		decision.ProvisioningBlocked = true
		decision.Reason = "stale_workload_uid"
		status := trainingpolicy.PolicyStatus(decision, r.now())
		status["message"] = err.Error()
		if err := patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusPolicyPath, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	capacity, err := r.checkCapacityLifecycle(ctx, policyObj, input, decision)
	if err != nil {
		return ctrl.Result{}, err
	}
	if capacity.Blocked {
		decision.ProvisioningBlocked = true
		decision.Reason = capacity.Reason
		status := trainingpolicy.PolicyStatus(decision, r.now())
		status["message"] = capacity.Message
		applyCapacityStatus(status, capacity)
		if err := patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusPolicyPath, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if err := r.ensureNodeProvisions(ctx, input, decision); err != nil {
		return ctrl.Result{}, err
	}
	status := trainingpolicy.PolicyStatus(decision, r.now())
	applyCapacityStatus(status, capacity)
	if err := patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusPolicyPath, status); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *PolicyReconciler) validateLiveWorkloadUID(ctx context.Context, input trainingpolicy.PolicyInput) error {
	return validateLiveWorkloadUID(ctx, r.Client, input)
}

func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("training-policy-management").
		For(trainingpolicy.NewObject("TrainingPolicy"), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(trainingpolicy.NewObject("SpotRiskProfile"), handler.EnqueueRequestsFromMapFunc(mapPolicies(r.Client, "risk"))).
		Watches(bindingObject(), handler.EnqueueRequestsFromMapFunc(mapPolicies(r.Client, "binding"))).
		Complete(r)
}

func (r *PolicyReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func validatePolicyInput(input trainingpolicy.PolicyInput) error {
	if input.TargetWorkers <= 0 {
		return fmt.Errorf("spec.targetWorkers must be positive")
	}
	if input.MinOnDemand > input.TargetWorkers {
		return fmt.Errorf("spec.policy.minOnDemand must be <= spec.targetWorkers")
	}
	if input.Checkpoint.MinIntervalSeconds > 0 && input.Checkpoint.MaxIntervalSeconds > 0 && input.Checkpoint.MinIntervalSeconds > input.Checkpoint.MaxIntervalSeconds {
		return fmt.Errorf("spec.checkpoint.minIntervalSeconds must be <= maxIntervalSeconds")
	}
	return nil
}

func validateRiskProfileMatchesCapacity(input trainingpolicy.PolicyInput, risk trainingpolicy.RiskSnapshot) error {
	if input.Capacity.Region != "" && risk.Region != "" && input.Capacity.Region != risk.Region {
		return fmt.Errorf("risk profile region %q does not match capacity region %q", risk.Region, input.Capacity.Region)
	}
	if input.Capacity.AvailabilityZone != "" && risk.AvailabilityZone != "" && input.Capacity.AvailabilityZone != risk.AvailabilityZone {
		return fmt.Errorf("risk profile availabilityZone %q does not match capacity availabilityZone %q", risk.AvailabilityZone, input.Capacity.AvailabilityZone)
	}
	if input.Capacity.InstanceType != "" && risk.InstanceType != "" && input.Capacity.InstanceType != risk.InstanceType {
		return fmt.Errorf("risk profile instanceType %q does not match capacity instanceType %q", risk.InstanceType, input.Capacity.InstanceType)
	}
	return nil
}

func (r *PolicyReconciler) runtimeSnapshot(ctx context.Context, input trainingpolicy.PolicyInput) (trainingpolicy.RuntimeSnapshot, error) {
	obj := trainingpolicy.NewObject("TrainingRuntime")
	err := r.Get(ctx, types.NamespacedName{Namespace: input.Namespace, Name: input.RuntimeRefName}, obj)
	if apierrors.IsNotFound(err) {
		return trainingpolicy.RuntimeSnapshot{}, nil
	}
	if err != nil {
		return trainingpolicy.RuntimeSnapshot{}, fmt.Errorf("get TrainingRuntime %s/%s: %w", input.Namespace, input.RuntimeRefName, err)
	}
	return trainingpolicy.ReadRuntimeStatus(obj, input.SourceCluster), nil
}

func (r *PolicyReconciler) riskSnapshot(ctx context.Context, input trainingpolicy.PolicyInput) (trainingpolicy.RiskSnapshot, error) {
	obj := trainingpolicy.NewObject("SpotRiskProfile")
	err := r.Get(ctx, types.NamespacedName{Namespace: input.Namespace, Name: input.RiskProfileName}, obj)
	if apierrors.IsNotFound(err) {
		return trainingpolicy.RiskSnapshot{}, nil
	}
	if err != nil {
		return trainingpolicy.RiskSnapshot{}, fmt.Errorf("get SpotRiskProfile %s/%s: %w", input.Namespace, input.RiskProfileName, err)
	}
	risk := trainingpolicy.ReadRiskStatus(obj)
	if expired(risk.ValidUntil, r.now()) {
		risk.Ready = false
	}
	return risk, nil
}

func (r *PolicyReconciler) ensureNodeProvisions(ctx context.Context, input trainingpolicy.PolicyInput, decision trainingpolicy.Decision) error {
	successors, err := r.completedReplacementSuccessors(ctx, input)
	if err != nil {
		return err
	}
	retired, err := r.retiredGeneratedSlots(ctx, input, successors)
	if err != nil {
		return err
	}
	for i := int64(0); i < decision.DesiredWorkers; i++ {
		desired := trainingpolicy.NewNodeProvision(input, i, desiredMarketForOrdinal(i, decision))
		if successors[desired.GetName()] != "" {
			continue
		}
		if retired[desired.GetName()] {
			continue
		}
		if err := r.createIfMissing(ctx, desired); err != nil {
			return err
		}
		placement := trainingpolicy.NewPropagationPolicyFor(input, desired, input.Capacity.AWSCluster)
		if err := r.createIfMissing(ctx, placement); err != nil {
			return err
		}
	}
	return nil
}

func (r *PolicyReconciler) createIfMissing(ctx context.Context, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create %s %s/%s: %w", desired.GetKind(), desired.GetNamespace(), desired.GetName(), err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s %s/%s: %w", desired.GetKind(), desired.GetNamespace(), desired.GetName(), err)
	}
	ownerUID := existing.GetLabels()[trainingpolicy.LabelPolicyUID]
	desiredUID := desired.GetLabels()[trainingpolicy.LabelPolicyUID]
	if ownerUID == "" || ownerUID != desiredUID {
		return fmt.Errorf("existing %s %s/%s owner uid %q does not match desired policy uid %q", desired.GetKind(), desired.GetNamespace(), desired.GetName(), ownerUID, desiredUID)
	}
	if desired.GetKind() == "NodeProvision" {
		existingMarket, _, _ := unstructured.NestedString(existing.Object, "spec", "marketType")
		desiredMarket, _, _ := unstructured.NestedString(desired.Object, "spec", "marketType")
		if existingMarket != "" && desiredMarket != "" && existingMarket != desiredMarket {
			return fmt.Errorf("existing NodeProvision %s/%s is immutable marketType=%s; desired=%s requires replacement operation/manual migration gate", desired.GetNamespace(), desired.GetName(), existingMarket, desiredMarket)
		}
	}
	if desired.GetKind() == "PropagationPolicy" && !samePlacementSpec(existing, desired) {
		return fmt.Errorf("existing PropagationPolicy %s/%s selector or placement drift requires manual intervention", desired.GetNamespace(), desired.GetName())
	}
	return nil
}

func samePlacementSpec(existing, desired *unstructured.Unstructured) bool {
	return reflect.DeepEqual(normalizedPlacementSpec(existing), normalizedPlacementSpec(desired))
}

func normalizedPlacementSpec(obj *unstructured.Unstructured) map[string]interface{} {
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if spec == nil {
		return nil
	}
	// Normalize only known admission defaults; retain every other field for drift checks.
	for key, value := range map[string]interface{}{
		"conflictResolution": "Abort", "preemption": "Never",
		"priority": int64(0), "schedulerName": "default-scheduler",
	} {
		if _, exists := spec[key]; !exists {
			spec[key] = value
		}
	}
	selectors, _ := spec["resourceSelectors"].([]interface{})
	for _, item := range selectors {
		if selector, ok := item.(map[string]interface{}); ok {
			if _, exists := selector["namespace"]; !exists {
				selector["namespace"] = obj.GetNamespace()
			}
		}
	}
	return spec
}

func (r *PolicyReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return defaultClock()
}
