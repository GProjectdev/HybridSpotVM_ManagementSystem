package management

import (
	"context"
	"errors"
	"fmt"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const partialFallbackHandoffRecorded = "HandoffRecorded"

var errPartialFallbackNotApplicable = errors.New("partial fallback not applicable")

type partialFallbackDecision struct {
	Reason              string
	Message             string
	CheckpointRef       map[string]interface{}
	FailedCheckpointRef map[string]interface{}
}

type partialFallbackHandoffError struct {
	fallback map[string]interface{}
	message  string
}

func (e *partialFallbackHandoffError) Error() string { return e.message }

func (r *ReplacementReconciler) reconcilePartialToGroupFallback(ctx context.Context, op *unstructured.Unstructured, spec replacementSpec, oldNP *unstructured.Unstructured) (string, bool, error) {
	fallback, recorded, _ := unstructured.NestedMap(op.Object, "status", "partialFallback")
	if !recorded {
		decision, eligible, err := r.classifyPartialToGroupFallback(ctx, op.GetNamespace(), spec)
		if err != nil {
			return replacementPhaseAwaitingPartialCheckpoint, true, err
		}
		if !eligible {
			return "", false, nil
		}
		fallback = map[string]interface{}{
			"phase":         partialFallbackHandoffRecorded,
			"reason":        decision.Reason,
			"message":       decision.Message,
			"checkpointRef": decision.CheckpointRef,
			"observedAt":    r.now().UTC().Format(time.RFC3339),
		}
		if decision.FailedCheckpointRef != nil {
			fallback["failedPartialCheckpointRef"] = decision.FailedCheckpointRef
		}
		return replacementPhaseAwaitingGroupFallback, true, &partialFallbackHandoffError{fallback: fallback, message: "partial-to-full-group fallback handoff recorded; waiting one reconcile before group restore"}
	}
	if stringField(fallback, "phase") != partialFallbackHandoffRecorded {
		return replacementPhaseAwaitingGroupFallback, true, fmt.Errorf("partial fallback handoff phase %q is not supported", stringField(fallback, "phase"))
	}
	if stringField(fallback, "checkpointRef", "name") == "" || stringField(fallback, "checkpointRef", "uid") == "" || intField(fallback, "checkpointRef", "generation") <= 0 || stringField(fallback, "checkpointRef", "checkpointID") == "" {
		return replacementPhaseAwaitingGroupFallback, true, fmt.Errorf("partial fallback checkpointRef is incomplete")
	}
	if err := r.validateRecordedPartialFallback(ctx, op.GetNamespace(), spec, fallback); err != nil {
		return replacementPhaseAwaitingGroupFallback, true, err
	}

	policy, input, err := r.fallbackPolicyInput(ctx, op.GetNamespace(), spec)
	if err != nil {
		return replacementPhaseAwaitingGroupFallback, true, err
	}
	requestName := spec.Operation + "-group-restore"
	request := newRestoreRequest()
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: requestName}, request); err == nil {
		if err := verifyGroupFallbackRequest(request, spec, fallback); err != nil {
			return replacementPhaseAwaitingGroupFallback, true, err
		}
		if stringField(request.Object, "status", "phase") == "Verified" {
			if err := validateGroupVerified(request); err != nil {
				return replacementPhaseAwaitingGroupFallback, true, err
			}
			runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
			if err := r.reader().Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: input.RuntimeRefName}, runtimeObj); err != nil {
				return replacementPhaseAwaitingGroupFallback, true, err
			}
			replacement := trainingpolicy.NewObject("NodeProvision")
			if err := r.reader().Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: spec.ReplacementNodeProvisionName}, replacement); err != nil {
				return replacementPhaseAwaitingGroupFallback, true, err
			}
			if err := verifyReplacementReadyForOperation(replacement, spec); err != nil {
				return replacementPhaseAwaitingGroupFallback, true, err
			}
			recoverySpec := groupFallbackRecoverySpec(spec, runtimeObj, request, oldNP, replacement, fallback)
			recovery, created, err := r.ensureSpotRecovery(ctx, op.GetNamespace(), spec, recoverySpec)
			if err != nil {
				return replacementPhaseAwaitingGroupFallback, true, err
			}
			if created {
				return replacementPhaseAwaitingGroupFallback, true, fmt.Errorf("waiting for SpotRecovery cleanup gate")
			}
			switch phase := stringField(recovery.Object, "status", "phase"); phase {
			case "Completed":
				return "Completed", true, nil
			case "Rejected":
				return "Rejected", true, fmt.Errorf("SpotRecovery cleanup rejected: %s", stringField(recovery.Object, "status", "message"))
			default:
				return replacementPhaseAwaitingGroupFallback, true, fmt.Errorf("waiting for SpotRecovery cleanup completion")
			}
		}
		return replacementPhaseAwaitingGroupFallback, true, fmt.Errorf("waiting for full-group RestoreRequest %s verification", requestName)
	} else if err != nil && !apierrors.IsNotFound(err) {
		return replacementPhaseAwaitingGroupFallback, true, err
	}

	if err := r.revalidateFreshPartialFallbackProof(ctx, op.GetNamespace(), spec, input, fallback); err != nil {
		return replacementPhaseAwaitingGroupFallback, true, err
	}
	round, err := selectGroupCheckpoint(ctx, r.reader(), input)
	if err != nil {
		return replacementPhaseAwaitingGroupFallback, true, err
	}
	if !fallbackCheckpointMatchesRound(fallback, round) {
		return replacementPhaseAwaitingGroupFallback, true, fmt.Errorf("durable full-group checkpoint changed after partial fallback handoff; refusing to switch recovery points")
	}
	if err := r.prepareReplacementForGroupFallback(ctx, op.GetNamespace(), spec, oldNP); err != nil {
		return replacementPhaseAwaitingGroupFallback, true, err
	}
	producer := &PolicyReconciler{Client: r.Client, APIReader: r.reader(), Clock: r.Clock}
	_, err = producer.ensureGroupReplacement(ctx, policy, input, oldNP, spec.Operation, spec.ReplacementNodeProvisionName, spec.DesiredMarketType, spec.EmergencyEventID)
	if err != nil {
		return replacementPhaseAwaitingGroupFallback, true, err
	}
	return replacementPhaseAwaitingGroupFallback, true, fmt.Errorf("waiting for full-group restore producer")
}

func (r *ReplacementReconciler) classifyPartialToGroupFallback(ctx context.Context, ns string, spec replacementSpec) (partialFallbackDecision, bool, error) {
	existingRestore := newRestoreRequest()
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.Operation + "-restore"}, existingRestore); err == nil {
		return partialFallbackDecision{}, false, nil
	} else if !apierrors.IsNotFound(err) {
		return partialFallbackDecision{}, false, err
	}
	partial := trainingpolicy.NewObject("FluidCRMigration")
	partialErr := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.Operation + "-partial-checkpoint"}, partial)
	if partialErr != nil && !apierrors.IsNotFound(partialErr) {
		return partialFallbackDecision{}, false, partialErr
	}
	_, input, err := r.fallbackPolicyInput(ctx, ns, spec)
	if err != nil {
		if errors.Is(err, errPartialFallbackNotApplicable) {
			return partialFallbackDecision{}, false, nil
		}
		return partialFallbackDecision{}, false, err
	}
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: input.RuntimeRefName}, runtimeObj); err != nil {
		return partialFallbackDecision{}, false, err
	}
	health := trainingpolicy.ReadRuntimeStatus(runtimeObj, input.SourceCluster)
	if !trainingpolicy.RuntimeReadyForCheckpoint(input, health, r.now()) {
		return partialFallbackDecision{}, false, nil
	}
	sources, err := (&PolicyReconciler{Client: r.Client, APIReader: r.reader(), Clock: r.Clock}).groupSourcePods(ctx, input)
	if err != nil {
		return partialFallbackDecision{}, false, err
	}
	if partialErr == nil {
		return r.classifyFailedPartialToGroupFallback(ctx, partial, spec, input, sources)
	}
	reason, message := partialFallbackLossReason(spec, sources)
	if reason == "" {
		return partialFallbackDecision{}, false, nil
	}
	round, err := selectGroupCheckpoint(ctx, r.reader(), input)
	if err != nil {
		return partialFallbackDecision{}, true, err
	}
	return partialFallbackDecision{Reason: reason, Message: message, CheckpointRef: groupCheckpointRef(round)}, true, nil
}

func (r *ReplacementReconciler) classifyFailedPartialToGroupFallback(ctx context.Context, partial *unstructured.Unstructured, spec replacementSpec, input trainingpolicy.PolicyInput, sources []interface{}) (partialFallbackDecision, bool, error) {
	cluster, ok := sourceClusterStatus(partial, spec.SourceCluster)
	if !ok || intField(cluster, "observedGeneration") != partial.GetGeneration() || stringField(cluster, "phase") != "Failed" {
		return partialFallbackDecision{}, false, nil
	}
	failedPods, ok := mapSliceFromStatus(cluster, "pods")
	if !ok || len(failedPods) == 0 {
		return partialFallbackDecision{}, false, fmt.Errorf("partial checkpoint Failed but source report lacks original pod identities; holding partial fallback")
	}
	originals := originalPartialParticipants(spec)
	if len(originals) == 0 || int64(len(originals)) != input.TargetWorkers {
		return partialFallbackDecision{}, false, fmt.Errorf("partial checkpoint Failed but fallback requires every original participant identity")
	}
	failedByRank := map[int64]map[string]interface{}{}
	for _, pod := range failedPods {
		failedByRank[intField(pod, "rank")] = pod
	}
	currentByRank := map[int64]map[string]interface{}{}
	for _, raw := range sources {
		pod := raw.(map[string]interface{})
		currentByRank[intField(pod, "rank")] = pod
	}
	for _, original := range originals {
		rank := intField(original, "rank")
		failed := failedByRank[rank]
		if failed == nil || firstString(failed, [][]string{{"podName"}, {"name"}, {"sourcePod"}}) != stringField(original, "podName") || firstString(failed, [][]string{{"podUID"}, {"uid"}, {"sourcePodUID"}}) != stringField(original, "podUID") {
			return partialFallbackDecision{}, false, fmt.Errorf("partial checkpoint Failed but failed report does not bind original rank %d identity", rank)
		}
		current := currentByRank[rank]
		if current == nil || stringField(current, "podName") != stringField(original, "podName") || stringField(current, "podUID") == "" || stringField(current, "podUID") == stringField(original, "podUID") {
			return partialFallbackDecision{}, false, fmt.Errorf("partial checkpoint Failed but original rank %d is not proven replaced by a fresh live UID", rank)
		}
	}
	round, err := selectGroupCheckpoint(ctx, r.reader(), input)
	if err != nil {
		return partialFallbackDecision{}, true, err
	}
	return partialFallbackDecision{
		Reason:              "failed_partial_all_originals_replaced",
		Message:             "partial checkpoint Failed with current fresh source snapshot proving every original participant UID was replaced",
		CheckpointRef:       groupCheckpointRef(round),
		FailedCheckpointRef: failedPartialCheckpointRef(partial),
	}, true, nil
}

func originalPartialParticipants(spec replacementSpec) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(spec.Pods)+len(spec.PreservedSurvivors))
	seen := map[int64]bool{}
	for _, raw := range spec.Pods {
		pod := raw.(map[string]interface{})
		rank := intField(pod, "rank")
		seen[rank] = true
		out = append(out, map[string]interface{}{"rank": rank, "podName": firstString(pod, [][]string{{"sourcePod"}, {"sourcePodName"}}), "podUID": stringField(pod, "sourcePodUID")})
	}
	for _, raw := range spec.PreservedSurvivors {
		survivor := raw.(map[string]interface{})
		rank := intField(survivor, "rank")
		if seen[rank] {
			continue
		}
		out = append(out, map[string]interface{}{"rank": rank, "podName": stringField(survivor, "podName"), "podUID": stringField(survivor, "podUID")})
	}
	return out
}

func groupFallbackRecoverySpec(spec replacementSpec, runtimeObj, restore, oldNP, replacement *unstructured.Unstructured, fallback map[string]interface{}) recoverySpec {
	return recoverySpec{
		PolicyName:                   spec.PolicyName,
		PolicyUID:                    spec.PolicyUID,
		PolicyGeneration:             spec.PolicyGeneration,
		RequestUID:                   string(restore.GetUID()),
		CheckpointID:                 stringField(fallback, "checkpointRef", "checkpointID"),
		EventID:                      stringField(oldNP.Object, "status", "spot", "eventID"),
		EmergencyEventID:             spec.EmergencyEventID,
		OldMarketType:                stringField(oldNP.Object, "spec", "marketType"),
		DesiredMarketType:            spec.DesiredMarketType,
		Operation:                    spec.Operation,
		SourceCluster:                spec.SourceCluster,
		TargetCluster:                spec.TargetCluster,
		WorkloadUID:                  spec.WorkloadUID,
		TrainingRuntimeName:          runtimeObj.GetName(),
		TrainingRuntimeUID:           string(runtimeObj.GetUID()),
		RestoreRequestName:           restore.GetName(),
		RestoreRequestUID:            string(restore.GetUID()),
		RestoreRequestGeneration:     restore.GetGeneration(),
		OldNodeProvisionName:         spec.OldNodeProvisionName,
		OldNodeProvisionUID:          spec.OldNodeProvisionUID,
		ReplacementNodeProvisionName: spec.ReplacementNodeProvisionName,
		ReplacementNodeProvisionUID:  string(replacement.GetUID()),
	}
}
func failedPartialCheckpointRef(partial *unstructured.Unstructured) map[string]interface{} {
	return map[string]interface{}{"name": partial.GetName(), "uid": string(partial.GetUID()), "generation": partial.GetGeneration(), "checkpointID": partial.GetAnnotations()["training.dcnlab.com/checkpoint-id"]}
}

func (r *ReplacementReconciler) validateRecordedPartialFallback(ctx context.Context, ns string, spec replacementSpec, fallback map[string]interface{}) error {
	restore := newRestoreRequest()
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.Operation + "-restore"}, restore); err == nil {
		return fmt.Errorf("partial RestoreRequest exists after fallback handoff; refusing group production")
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	partial := trainingpolicy.NewObject("FluidCRMigration")
	if stringField(fallback, "failedPartialCheckpointRef", "name") == "" {
		if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.Operation + "-partial-checkpoint"}, partial); err == nil {
			return fmt.Errorf("partial checkpoint exists after fallback handoff; refusing group production")
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.Operation + "-partial-checkpoint"}, partial); err != nil {
		return err
	}
	if string(partial.GetUID()) != stringField(fallback, "failedPartialCheckpointRef", "uid") || partial.GetGeneration() != intField(fallback, "failedPartialCheckpointRef", "generation") || partial.GetAnnotations()["training.dcnlab.com/checkpoint-id"] != stringField(fallback, "failedPartialCheckpointRef", "checkpointID") {
		return fmt.Errorf("failed partial checkpoint identity changed after fallback handoff")
	}
	cluster, ok := sourceClusterStatus(partial, spec.SourceCluster)
	if !ok || intField(cluster, "observedGeneration") != partial.GetGeneration() || stringField(cluster, "phase") != "Failed" {
		return fmt.Errorf("failed partial checkpoint is no longer a current terminal Failed round")
	}
	return nil
}
func (r *ReplacementReconciler) revalidateFreshPartialFallbackProof(ctx context.Context, ns string, spec replacementSpec, input trainingpolicy.PolicyInput, fallback map[string]interface{}) error {
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: input.RuntimeRefName}, runtimeObj); err != nil {
		return err
	}
	health := trainingpolicy.ReadRuntimeStatus(runtimeObj, input.SourceCluster)
	if !trainingpolicy.RuntimeReadyForCheckpoint(input, health, r.now()) {
		return fmt.Errorf("fresh runtime snapshot no longer proves partial fallback is safe")
	}
	sources, err := (&PolicyReconciler{Client: r.Client, APIReader: r.reader(), Clock: r.Clock}).groupSourcePods(ctx, input)
	if err != nil {
		return err
	}
	if stringField(fallback, "failedPartialCheckpointRef", "name") == "" {
		reason, _ := partialFallbackLossReason(spec, sources)
		if reason == "" || reason != stringField(fallback, "reason") {
			return fmt.Errorf("fresh source snapshot no longer proves recorded partial fallback reason")
		}
		return nil
	}
	partial := trainingpolicy.NewObject("FluidCRMigration")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.Operation + "-partial-checkpoint"}, partial); err != nil {
		return err
	}
	decision, eligible, err := r.classifyFailedPartialToGroupFallback(ctx, partial, spec, input, sources)
	if err != nil {
		return err
	}
	if !eligible || decision.Reason != stringField(fallback, "reason") || !fallbackCheckpointRefMatchesDecision(fallback, decision) || !fallbackFailedCheckpointRefMatchesDecision(fallback, decision) {
		return fmt.Errorf("fresh source snapshot no longer proves recorded failed partial fallback")
	}
	return nil
}

func fallbackCheckpointRefMatchesDecision(fallback map[string]interface{}, decision partialFallbackDecision) bool {
	for _, field := range []string{"name", "uid", "checkpointID"} {
		if stringField(fallback, "checkpointRef", field) != stringField(decision.CheckpointRef, field) {
			return false
		}
	}
	return intField(fallback, "checkpointRef", "generation") == intField(decision.CheckpointRef, "generation")
}

func fallbackFailedCheckpointRefMatchesDecision(fallback map[string]interface{}, decision partialFallbackDecision) bool {
	for _, field := range []string{"name", "uid", "checkpointID"} {
		if stringField(fallback, "failedPartialCheckpointRef", field) != stringField(decision.FailedCheckpointRef, field) {
			return false
		}
	}
	return intField(fallback, "failedPartialCheckpointRef", "generation") == intField(decision.FailedCheckpointRef, "generation")
}
func (r *ReplacementReconciler) fallbackPolicyInput(ctx context.Context, ns string, spec replacementSpec) (*unstructured.Unstructured, trainingpolicy.PolicyInput, error) {
	policy := trainingpolicy.NewObject("TrainingPolicy")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.PolicyName}, policy); err != nil {
		return nil, trainingpolicy.PolicyInput{}, fmt.Errorf("get TrainingPolicy for fallback: %w", err)
	}
	if string(policy.GetUID()) != spec.PolicyUID || policy.GetGeneration() != spec.PolicyGeneration || stringField(policy.Object, "spec", "workloadRef", "uid") != spec.WorkloadUID {
		return nil, trainingpolicy.PolicyInput{}, fmt.Errorf("fallback policy identity does not match SpotReplacement")
	}
	input := trainingpolicy.ReadPolicyInput(policy)
	if input.SourceCluster != spec.SourceCluster || input.Capacity.AWSCluster != spec.TargetCluster || input.WorkloadRef.UID != types.UID(spec.WorkloadUID) {
		return nil, trainingpolicy.PolicyInput{}, errPartialFallbackNotApplicable
	}
	return policy, input, nil
}

func (r *ReplacementReconciler) prepareReplacementForGroupFallback(ctx context.Context, ns string, spec replacementSpec, oldNP *unstructured.Unstructured) error {
	replacement := trainingpolicy.NewObject("NodeProvision")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.ReplacementNodeProvisionName}, replacement); err != nil {
		return err
	}
	if replacement.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID || stringField(replacement.Object, "spec", "marketType") != spec.DesiredMarketType || string(replacement.GetUID()) == spec.OldNodeProvisionUID {
		return fmt.Errorf("replacement NodeProvision identity mismatch before group fallback")
	}
	if op := replacement.GetAnnotations()["training.dcnlab.com/recovery-operation"]; op != spec.Operation {
		return fmt.Errorf("replacement NodeProvision operation mismatch before group fallback")
	}
	if existing := replacement.GetAnnotations()[groupOldUID]; existing == string(oldNP.GetUID()) {
		return nil
	} else if existing != "" {
		return fmt.Errorf("replacement NodeProvision group fallback source UID conflict")
	}
	before := replacement.DeepCopy()
	annotations := replacement.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[groupOldUID] = string(oldNP.GetUID())
	replacement.SetAnnotations(annotations)
	return r.Client.Patch(ctx, replacement, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func partialFallbackLossReason(spec replacementSpec, sources []interface{}) (string, string) {
	byRank := map[int64]map[string]interface{}{}
	for _, raw := range sources {
		pod, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		byRank[intField(pod, "rank")] = pod
	}
	for _, raw := range spec.Pods {
		pod, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		rank := intField(pod, "rank")
		current := byRank[rank]
		wantName := firstString(pod, [][]string{{"sourcePod"}, {"sourcePodName"}})
		wantUID := stringField(pod, "sourcePodUID")
		if current != nil && (stringField(current, "podName") != wantName || stringField(current, "podUID") != wantUID) {
			return "target_source_pod_replaced", fmt.Sprintf("fresh source snapshot proves target rank %d changed from pod %s uid %s to pod %s uid %s", rank, wantName, wantUID, stringField(current, "podName"), stringField(current, "podUID"))
		}
	}
	for _, raw := range spec.PreservedSurvivors {
		survivor, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		rank := intField(survivor, "rank")
		current := byRank[rank]
		wantName := stringField(survivor, "podName")
		wantUID := stringField(survivor, "podUID")
		if current != nil && (stringField(current, "podName") != wantName || stringField(current, "podUID") != wantUID) {
			return "survivor_pod_replaced", fmt.Sprintf("fresh source snapshot proves survivor rank %d changed from pod %s uid %s to pod %s uid %s", rank, wantName, wantUID, stringField(current, "podName"), stringField(current, "podUID"))
		}
	}
	return "", ""
}

func groupCheckpointRef(round *groupCheckpoint) map[string]interface{} {
	return map[string]interface{}{
		"name":         round.Object.GetName(),
		"uid":          string(round.Object.GetUID()),
		"generation":   round.Object.GetGeneration(),
		"checkpointID": round.Object.GetAnnotations()["training.dcnlab.com/checkpoint-id"],
	}
}

func fallbackCheckpointMatchesRound(fallback map[string]interface{}, round *groupCheckpoint) bool {
	return stringField(fallback, "checkpointRef", "name") == round.Object.GetName() &&
		stringField(fallback, "checkpointRef", "uid") == string(round.Object.GetUID()) &&
		intField(fallback, "checkpointRef", "generation") == round.Object.GetGeneration() &&
		stringField(fallback, "checkpointRef", "checkpointID") == round.Object.GetAnnotations()["training.dcnlab.com/checkpoint-id"]
}

func verifyGroupFallbackRequest(req *unstructured.Unstructured, spec replacementSpec, fallback map[string]interface{}) error {
	if req.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID || req.GetLabels()[trainingpolicy.LabelRole] != groupRestoreRole {
		return fmt.Errorf("group fallback RestoreRequest ownership mismatch")
	}
	if stringField(req.Object, "spec", "groupRestore", "operationUID") != spec.OldNodeProvisionUID || stringField(req.Object, "spec", "workloadRef", "uid") != spec.WorkloadUID {
		return fmt.Errorf("group fallback RestoreRequest operation/workload mismatch")
	}
	for _, field := range []string{"name", "uid", "checkpointID"} {
		if stringField(req.Object, "spec", "checkpointRef", field) != stringField(fallback, "checkpointRef", field) {
			return fmt.Errorf("group fallback RestoreRequest checkpointRef %s mismatch", field)
		}
	}
	if intField(req.Object, "spec", "checkpointRef", "generation") != intField(fallback, "checkpointRef", "generation") {
		return fmt.Errorf("group fallback RestoreRequest checkpointRef generation mismatch")
	}
	return nil
}
