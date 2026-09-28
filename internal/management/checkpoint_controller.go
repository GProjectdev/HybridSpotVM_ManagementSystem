package management

import (
	"context"
	"fmt"
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

type CheckpointReconciler struct {
	client.Client
	Clock func() time.Time
}

func (r *CheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	policyObj := trainingpolicy.NewObject("TrainingPolicy")
	if err := r.Get(ctx, req.NamespacedName, policyObj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !policyObj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	input := trainingpolicy.ReadPolicySpec(policyObj)
	if operation := policyObj.GetAnnotations()[groupIntentAnnotation]; operation != "" {
		_, inflight, _, _, err := r.checkpointState(ctx, input)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !inflight {
			if _, err := r.ensureEmergencyReplacement(ctx, policyObj, input); err != nil {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
		reason := "group_intent_quiesced"
		if inflight {
			reason = "group_intent_waiting_checkpoint"
		}
		status := trainingpolicy.CheckpointStatus("", 0, r.now(), reason)
		status["periodicQuiesced"] = !inflight
		status["replacementOperation"] = operation
		return ctrl.Result{RequeueAfter: 5 * time.Second}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	if group, err := activeGroupRestore(ctx, r.Client, input); err != nil {
		return ctrl.Result{}, err
	} else if group != nil {
		status := trainingpolicy.CheckpointStatus("", 0, r.now(), "group_restore_active")
		status["periodicQuiesced"] = true
		status["replacementOperation"] = group.GetName()
		return ctrl.Result{RequeueAfter: 5 * time.Second}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	if policyObj.GetLabels()[autoLabel] == "true" {
		ready, _, _ := unstructured.NestedBool(policyObj.Object, "status", "discovery", "ready")
		if !ready {
			return ctrl.Result{RequeueAfter: 15 * time.Second}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, trainingpolicy.CheckpointStatus("", 0, r.now(), "waiting_for_discovery"))
		}
	}
	if err := validatePolicyInput(input); err != nil {
		status := trainingpolicy.CheckpointStatus("", 0, r.now(), "invalid_spec")
		status["message"] = err.Error()
		return ctrl.Result{}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	lastStarted, inflight, inflightObj, measuredCosts, err := r.checkpointState(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	if inflight {
		if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, inflightObj, input.SourceCluster)); err != nil {
			return ctrl.Result{}, err
		}
	}
	operation, emergencyErr := r.ensureEmergencyReplacement(ctx, policyObj, input)
	if operation != "" || emergencyErr != nil {
		status := trainingpolicy.CheckpointStatus("", 0, r.now(), "emergency_replacement_active")
		status["periodicQuiesced"] = true
		status["replacementOperation"] = operation
		if emergencyErr != nil {
			status["reason"] = "emergency_replacement_blocked"
			status["message"] = emergencyErr.Error()
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	if inflight {
		placement := trainingpolicy.NewPropagationPolicyFor(input, inflightObj, input.SourceCluster)
		if err := r.createIfMissing(ctx, placement); err != nil {
			return ctrl.Result{}, err
		}
		status := trainingpolicy.CheckpointStatus(inflightObj.GetName(), trainingpolicy.DefaultCheckpointSeconds, r.now(), "inflight_checkpoint")
		applyMeasuredCostsStatus(status, measuredCosts)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	runtimeSnapshot, err := r.runtimeSnapshot(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := r.now()
	replacement, err := r.activeSpotReplacement(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	if replacement != nil {
		status := trainingpolicy.CheckpointStatus("", trainingpolicy.DefaultCheckpointSeconds, now, "replacement_operation_active")
		applyMeasuredCostsStatus(status, measuredCosts)
		status["replacementOperation"] = replacement.GetName()
		status["periodicQuiesced"] = true
		status["message"] = "periodic checkpoints quiesced while SpotReplacement reconciler owns the typed partial-rank checkpoint"
		return ctrl.Result{RequeueAfter: time.Minute}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	emergency, err := r.unhandledEmergencySpotEvent(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	if emergency != nil {
		interval := trainingpolicy.Decide(input, runtimeSnapshot, trainingpolicy.RiskSnapshot{}).CheckpointIntervalSeconds
		if !trainingpolicy.RuntimeReadyForCheckpoint(input, runtimeSnapshot, now) {
			status := trainingpolicy.CheckpointStatus("", interval, now, "runtime_not_ready")
			applyMeasuredCostsStatus(status, measuredCosts)
			return ctrl.Result{RequeueAfter: time.Minute}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
		}
		if err := r.validateLiveWorkloadUID(ctx, input); err != nil {
			status := trainingpolicy.CheckpointStatus("", interval, now, "stale_workload_uid")
			applyMeasuredCostsStatus(status, measuredCosts)
			status["message"] = err.Error()
			return ctrl.Result{RequeueAfter: time.Minute}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
		}
		if existing, err := r.emergencyCheckpoint(ctx, input, *emergency); err != nil {
			return ctrl.Result{}, err
		} else if existing != nil {
			placement := trainingpolicy.NewPropagationPolicyFor(input, existing, input.SourceCluster)
			if err := r.createIfMissing(ctx, placement); err != nil {
				return ctrl.Result{}, err
			}
			status := trainingpolicy.CheckpointStatus(existing.GetName(), interval, now, "emergency_checkpoint_deduped")
			applyMeasuredCostsStatus(status, measuredCosts)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
		}
		migration := newEmergencyCheckpoint(input, runtimeSnapshot, *emergency, now, interval)
		if err := r.createIfMissing(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		placement := trainingpolicy.NewPropagationPolicyFor(input, migration, input.SourceCluster)
		if err := r.createIfMissing(ctx, placement); err != nil {
			return ctrl.Result{}, err
		}
		status := trainingpolicy.CheckpointStatus(migration.GetName(), interval, now, "emergency_checkpoint_created")
		applyMeasuredCostsStatus(status, measuredCosts)
		if err := patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Duration(interval) * time.Second}, nil
	}
	riskSnapshot, err := r.riskSnapshot(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	decision := trainingpolicy.Decide(input, runtimeSnapshot, riskSnapshot)
	if !trainingpolicy.RiskFreshForPolicy(input, riskSnapshot, nowOr(r.Clock)) {
		status := trainingpolicy.CheckpointStatus("", decision.CheckpointIntervalSeconds, r.now(), "risk_not_fresh")
		applyMeasuredCostsStatus(status, measuredCosts)
		return ctrl.Result{RequeueAfter: time.Minute}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	if !trainingpolicy.RuntimeReadyForCheckpoint(input, runtimeSnapshot, now) {
		status := trainingpolicy.CheckpointStatus("", decision.CheckpointIntervalSeconds, now, "runtime_not_ready")
		applyMeasuredCostsStatus(status, measuredCosts)
		return ctrl.Result{RequeueAfter: time.Minute}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	if err := r.validateLiveWorkloadUID(ctx, input); err != nil {
		status := trainingpolicy.CheckpointStatus("", decision.CheckpointIntervalSeconds, now, "stale_workload_uid")
		applyMeasuredCostsStatus(status, measuredCosts)
		status["message"] = err.Error()
		return ctrl.Result{RequeueAfter: time.Minute}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	if !trainingpolicy.NextCheckpointDue(now, lastStarted, decision.CheckpointIntervalSeconds) {
		status := trainingpolicy.CheckpointStatus("", decision.CheckpointIntervalSeconds, now, "waiting_for_interval")
		applyMeasuredCostsStatus(status, measuredCosts)
		return ctrl.Result{RequeueAfter: checkpointWaitRequeue(now, lastStarted, decision.CheckpointIntervalSeconds)}, patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status)
	}
	startedAt := now
	migration := trainingpolicy.NewFluidCRMigration(input, runtimeSnapshot, startedAt, decision.CheckpointIntervalSeconds)
	if err := r.createIfMissing(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	placement := trainingpolicy.NewPropagationPolicyFor(input, migration, input.SourceCluster)
	if err := r.createIfMissing(ctx, placement); err != nil {
		return ctrl.Result{}, err
	}
	status := trainingpolicy.CheckpointStatus(migration.GetName(), decision.CheckpointIntervalSeconds, now, "checkpoint_created")
	applyMeasuredCostsStatus(status, measuredCosts)
	if err := patchStatusSubtree(ctx, r.Client, policyObj, trainingpolicy.StatusCheckpointPath, status); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Duration(decision.CheckpointIntervalSeconds) * time.Second}, nil
}

func (r *CheckpointReconciler) validateLiveWorkloadUID(ctx context.Context, input trainingpolicy.PolicyInput) error {
	return validateLiveWorkloadUID(ctx, r.Client, input)
}

func (r *CheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("training-checkpoint-management").
		For(trainingpolicy.NewObject("TrainingPolicy"), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(trainingpolicy.NewObject("SpotRiskProfile"), handler.EnqueueRequestsFromMapFunc(mapPolicies(r.Client, "risk"))).
		Watches(trainingpolicy.NewObject("TrainingRuntime"), handler.EnqueueRequestsFromMapFunc(mapPolicies(r.Client, "runtime"))).
		Watches(trainingpolicy.NewObject("NodeProvision"), handler.EnqueueRequestsFromMapFunc(mapPolicies(r.Client, "node"))).
		Watches(newSpotReplacementObject(), handler.EnqueueRequestsFromMapFunc(mapPolicies(r.Client, "replacement"))).
		Watches(bindingObject(), handler.EnqueueRequestsFromMapFunc(mapPolicies(r.Client, "binding"))).
		Complete(r)
}

func (r *CheckpointReconciler) activeSpotReplacement(ctx context.Context, input trainingpolicy.PolicyInput) (*unstructured.Unstructured, error) {
	list := newSpotReplacementList()
	labels := client.MatchingLabels{trainingpolicy.LabelPolicyUID: string(input.PolicyUID)}
	if err := r.List(ctx, list, client.InNamespace(input.Namespace), labels); err != nil {
		return nil, fmt.Errorf("list SpotReplacement operations: %w", err)
	}
	for i := range list.Items {
		item := list.Items[i].DeepCopy()
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if phase == "Completed" || phase == "Rejected" {
			continue
		}
		policyUID, _, _ := unstructured.NestedString(item.Object, "spec", "policyRef", "uid")
		if policyUID == string(input.PolicyUID) {
			return item, nil
		}
	}
	return nil, nil
}

func (r *CheckpointReconciler) checkpointState(ctx context.Context, input trainingpolicy.PolicyInput) (string, bool, *unstructured.Unstructured, *trainingpolicy.MeasuredCosts, error) {
	list := trainingpolicy.NewList("FluidCRMigration")
	labels := client.MatchingLabels{trainingpolicy.LabelPolicyUID: string(input.PolicyUID), trainingpolicy.LabelRole: "checkpoint"}
	if err := r.List(ctx, list, client.InNamespace(input.Namespace), labels); err != nil {
		return "", false, nil, nil, fmt.Errorf("list FluidCRMigration checkpoints: %w", err)
	}
	lastStarted := ""
	var latestMeasured *trainingpolicy.MeasuredCosts
	for i := range list.Items {
		item := &list.Items[i]
		if !isTerminalPhase(item, input.SourceCluster) {
			return lastStarted, true, item, latestMeasured, nil
		}
		if measured, ok, _ := measuredCostsFromMigration(item, input.SourceCluster, r.now()); ok {
			costs := measured
			if latestMeasured == nil || costs.ObservedAt > latestMeasured.ObservedAt {
				latestMeasured = &costs
			}
		}
		started := item.GetAnnotations()["training.dcnlab.com/started-at"]
		if started > lastStarted {
			lastStarted = started
		}
	}
	return lastStarted, false, nil, latestMeasured, nil
}

func applyMeasuredCostsStatus(status map[string]interface{}, measured *trainingpolicy.MeasuredCosts) {
	if measured == nil {
		return
	}
	status["measuredCosts"] = map[string]interface{}{
		"checkpointSeconds": measured.CheckpointSeconds,
		"copySeconds":       measured.CopySeconds,
		"observedAt":        measured.ObservedAt,
		"source":            "fluidcr-status",
	}
}

func (r *CheckpointReconciler) runtimeSnapshot(ctx context.Context, input trainingpolicy.PolicyInput) (trainingpolicy.RuntimeSnapshot, error) {
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

func (r *CheckpointReconciler) riskSnapshot(ctx context.Context, input trainingpolicy.PolicyInput) (trainingpolicy.RiskSnapshot, error) {
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

func (r *CheckpointReconciler) createIfMissing(ctx context.Context, desired client.Object) error {
	existing := desired.DeepCopyObject().(client.Object)
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err == nil {
		ownerUID := existing.GetLabels()[trainingpolicy.LabelPolicyUID]
		desiredUID := desired.GetLabels()[trainingpolicy.LabelPolicyUID]
		if ownerUID == "" || ownerUID != desiredUID {
			return fmt.Errorf("existing %T %s/%s owner uid %q does not match desired policy uid %q", desired, desired.GetNamespace(), desired.GetName(), ownerUID, desiredUID)
		}
		if desired.GetObjectKind().GroupVersionKind().Kind == "PropagationPolicy" {
			existingObj, existingOK := existing.(*unstructured.Unstructured)
			desiredObj, desiredOK := desired.(*unstructured.Unstructured)
			if existingOK && desiredOK && !samePlacementSpec(existingObj, desiredObj) {
				return fmt.Errorf("existing PropagationPolicy %s/%s selector or placement drift requires manual intervention", desired.GetNamespace(), desired.GetName())
			}
		}
	}
	return err
}

func (r *CheckpointReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return defaultClock()
}

func isTerminalPhase(obj client.Object, sourceCluster string) bool {
	unstructuredObj, ok := obj.(interface{ UnstructuredContent() map[string]interface{} })
	if !ok {
		return false
	}
	content := unstructuredObj.UnstructuredContent()
	phase := ""
	found := 0
	for _, cluster := range nestedClusterStatuses(content) {
		if name, _, _ := unstructured.NestedString(cluster, "clusterName"); name == sourceCluster {
			found++
			phase, _, _ = unstructured.NestedString(cluster, "phase")
			observedGeneration, _, _ := unstructured.NestedInt64(cluster, "observedGeneration")
			if generation := obj.GetGeneration(); generation > 0 && observedGeneration != generation {
				return false
			}
		}
	}
	if found != 1 {
		return false
	}
	switch phase {
	case "Completed", "Failed", "Cancelled":
		return true
	default:
		return false
	}
}

func nestedClusterStatuses(content map[string]interface{}) []map[string]interface{} {
	raw, ok, _ := unstructured.NestedSlice(content, "status", "clusters")
	if !ok {
		return nil
	}
	clusters := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		if typed, ok := item.(map[string]interface{}); ok {
			clusters = append(clusters, typed)
		}
	}
	return clusters
}

func deterministicStart(now time.Time, lastStarted string, intervalSeconds int64) time.Time {
	if intervalSeconds <= 0 || lastStarted == "" {
		return now
	}
	parsed, err := time.Parse(time.RFC3339, lastStarted)
	if err != nil {
		return now
	}
	next := parsed.Add(time.Duration(intervalSeconds) * time.Second)
	if next.After(now) {
		return now
	}
	return next
}

func checkpointWaitRequeue(now time.Time, lastStarted string, intervalSeconds int64) time.Duration {
	if intervalSeconds <= 0 || lastStarted == "" {
		return 5 * time.Second
	}
	parsed, err := time.Parse(time.RFC3339, lastStarted)
	if err != nil {
		return 5 * time.Second
	}
	remaining := parsed.Add(time.Duration(intervalSeconds) * time.Second).Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining < 5*time.Second {
		return remaining
	}
	return 5 * time.Second
}

func nowOr(clock func() time.Time) time.Time {
	if clock != nil {
		return clock()
	}
	return defaultClock()
}

func expired(validUntil string, now time.Time) bool {
	if validUntil == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, validUntil)
	return err != nil || now.After(parsed)
}
