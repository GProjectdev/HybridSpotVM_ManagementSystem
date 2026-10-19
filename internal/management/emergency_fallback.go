package management

import (
	"context"
	"fmt"
	"reflect"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const emergencyFallbackReason = "interruption_deadline_incomplete_partial"
const groupFallbackCancelAnnotation = "training.dcnlab.com/group-fallback-cancel"

func emergencyFallback(fallback map[string]interface{}) bool {
	return stringField(fallback, "reason") == emergencyFallbackReason
}

func (r *ReplacementReconciler) classifyEmergencyPartialFallback(ctx context.Context, ns string, spec replacementSpec, input p.PolicyInput, partial *unstructured.Unstructured, exists bool) (partialFallbackDecision, bool, error) {
	if spec.EmergencyEventID == "" || spec.EmergencyBinding[annotationEmergencyInstanceID] == "" {
		return partialFallbackDecision{}, false, nil
	}
	old := p.NewObject("NodeProvision")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.OldNodeProvisionName}, old); err != nil {
		return partialFallbackDecision{}, false, err
	}
	if err := verifyReplacementOldNode(old, spec); err != nil {
		return partialFallbackDecision{}, false, err
	}
	if stringField(old.Object, "status", "spot", "signalType") != spotInterruptionNotice {
		return partialFallbackDecision{}, false, nil
	}
	stamp := stringField(old.Object, "status", "spot", "interruptionTime")
	deadline, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || deadline.IsZero() {
		return partialFallbackDecision{}, false, nil
	}
	if r.now().Before(deadline) {
		return partialFallbackDecision{}, false, nil
	}
	if exists {
		if err := verifyReplacementCheckpointIdentity(partial, spec); err != nil {
			return partialFallbackDecision{}, false, err
		}
		if err := verifyPartialCheckpointEvidence(partial, spec); err == nil {
			return partialFallbackDecision{}, false, nil
		}
	}
	sources, err := (&PolicyReconciler{Client: r.Client, APIReader: r.reader(), Clock: r.Clock}).groupSourcePods(ctx, input)
	if err != nil {
		return partialFallbackDecision{}, true, err
	}
	if err := validateOriginalEmergencySources(spec, sources, input.TargetWorkers); err != nil {
		return partialFallbackDecision{}, true, err
	}
	round, err := selectGroupCheckpoint(ctx, r.reader(), input)
	if err != nil {
		return partialFallbackDecision{}, true, err
	}
	d := partialFallbackDecision{Reason: emergencyFallbackReason, Message: "interruption deadline reached without usable partial checkpoint; pinning complete group rollback", CheckpointRef: groupCheckpointRef(round), EmergencySources: sources, InterruptionTime: stamp}
	if exists {
		d.InterruptedCheckpointRef = failedPartialCheckpointRef(partial)
	}
	return d, true, nil
}

func validateOriginalEmergencySources(spec replacementSpec, sources []interface{}, world int64) error {
	originals := originalPartialParticipants(spec)
	if int64(len(sources)) != world || len(originals) != len(sources) {
		return fmt.Errorf("emergency fallback requires every original rank identity")
	}
	byRank := map[int64]map[string]interface{}{}
	for _, raw := range sources {
		s, ok := raw.(map[string]interface{})
		if !ok || byRank[intField(s, "rank")] != nil {
			return fmt.Errorf("invalid emergency source snapshot")
		}
		byRank[intField(s, "rank")] = s
	}
	for _, original := range originals {
		s := byRank[intField(original, "rank")]
		if stringField(original, "podUID") == "" || s == nil || stringField(s, "podUID") != stringField(original, "podUID") || stringField(s, "podName") != stringField(original, "podName") {
			return fmt.Errorf("emergency source rank identity changed")
		}
	}
	for _, raw := range spec.Pods {
		pod := raw.(map[string]interface{})
		s := byRank[intField(pod, "rank")]
		if stringField(s, "nodeName") != stringField(pod, "sourceNode") || stringField(s, "nodeProvisionRef", "name") != spec.OldNodeProvisionName || stringField(s, "nodeProvisionRef", "instanceID") != spec.EmergencyBinding[annotationEmergencyInstanceID] {
			return fmt.Errorf("emergency target source node identity changed")
		}
	}
	for _, raw := range spec.PreservedSurvivors {
		pod := raw.(map[string]interface{})
		if stringField(byRank[intField(pod, "rank")], "nodeName") != stringField(pod, "nodeName") {
			return fmt.Errorf("emergency survivor node identity changed")
		}
	}
	return nil
}

func (r *ReplacementReconciler) quiesceInterruptedPartial(ctx context.Context, ns string, spec replacementSpec, fallback map[string]interface{}) error {
	partial := p.NewObject("FluidCRMigration")
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.Operation + "-partial-checkpoint"}, partial)
	ref, recorded, _ := unstructured.NestedMap(fallback, "interruptedPartialCheckpointRef")
	if !recorded {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("partial checkpoint appeared after emergency handoff")
	}
	if err != nil {
		return fmt.Errorf("waiting for interrupted partial cancellation receipt: %w", err)
	}
	if err := verifyReplacementCheckpointIdentity(partial, spec); err != nil {
		return err
	}
	if !reflect.DeepEqual(failedPartialCheckpointRef(partial), ref) {
		return fmt.Errorf("interrupted partial checkpoint identity changed")
	}
	if partial.GetAnnotations()[groupFallbackCancelAnnotation] != spec.OperationUID {
		if partial.GetAnnotations()[groupFallbackCancelAnnotation] != "" {
			return fmt.Errorf("partial cancellation operation conflict")
		}
		before := partial.DeepCopy()
		a := partial.GetAnnotations()
		a[groupFallbackCancelAnnotation] = spec.OperationUID
		partial.SetAnnotations(a)
		if err := r.Patch(ctx, partial, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		return fmt.Errorf("waiting for member checkpoint cancellation acknowledgement")
	}
	status, ok := sourceClusterStatus(partial, spec.SourceCluster)
	if !ok || intField(status, "observedGeneration") != partial.GetGeneration() || stringField(status, "phase") != "Failed" || stringField(status, "message") != "group fallback checkpoint cancelled: "+spec.OperationUID {
		return fmt.Errorf("waiting for member checkpoint cancellation acknowledgement")
	}
	return nil
}

func (r *ReplacementReconciler) validateEmergencySources(ctx context.Context, input p.PolicyInput, spec replacementSpec, fallback map[string]interface{}) error {
	pinned, _, _ := unstructured.NestedSlice(fallback, "sourcePods")
	if err := validateOriginalEmergencySources(spec, pinned, input.TargetWorkers); err != nil {
		return err
	}
	sources, err := (&PolicyReconciler{Client: r.Client, APIReader: r.reader(), Clock: r.Clock}).groupSourcePods(ctx, input)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(pinned, sources) {
		return fmt.Errorf("source identity changed after emergency fallback handoff")
	}
	return nil
}

func fallbackGroupCheckpoint(ctx context.Context, reader client.Reader, input p.PolicyInput, fallback map[string]interface{}) (*groupCheckpoint, error) {
	if !emergencyFallback(fallback) {
		return selectGroupCheckpoint(ctx, reader, input)
	}
	cp := p.NewObject("FluidCRMigration")
	if err := reader.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: stringField(fallback, "checkpointRef", "name")}, cp); err != nil {
		return nil, err
	}
	round, err := readGroupCheckpoint(cp, input)
	if err != nil {
		return nil, err
	}
	if !fallbackCheckpointMatchesRound(fallback, round) {
		return nil, fmt.Errorf("pinned group checkpoint identity changed")
	}
	return round, nil
}

func verifyFallbackOldNode(old *unstructured.Unstructured, spec replacementSpec, fallback map[string]interface{}) error {
	if !emergencyFallback(fallback) {
		// A recorded emergency group handoff may already have fenced its source.
		if spec.EmergencyEventID == "" || stringField(fallback, "phase") != partialFallbackHandoffRecorded {
			return verifyReplacementOldNode(old, spec)
		}
		unfenced := old.DeepCopy()
		unstructured.RemoveNestedField(unfenced.Object, "spec", "fence")
		unstructured.RemoveNestedField(unfenced.Object, "status", "fence")
		if err := verifyReplacementOldNode(unfenced, spec); err != nil {
			return err
		}
	}
	if old.GetName() != spec.OldNodeProvisionName || string(old.GetUID()) != spec.OldNodeProvisionUID || old.GetLabels()[p.LabelPolicyUID] != spec.PolicyUID || !old.GetDeletionTimestamp().IsZero() ||
		spec.EmergencyBinding[annotationEmergencyInstanceID] == "" || stringField(old.Object, "status", "instanceId") != spec.EmergencyBinding[annotationEmergencyInstanceID] || stringField(old.Object, "spec", "marketType") != "Spot" || spec.DesiredMarketType != "OnDemand" {
		return fmt.Errorf("recorded emergency fallback source identity mismatch")
	}
	for _, path := range [][]string{{"spec", "fence"}, {"status", "fence"}} {
		if fence, found, _ := unstructured.NestedMap(old.Object, path...); found && (stringField(fence, "operationUID") != spec.OldNodeProvisionUID || stringField(fence, "instanceID") != spec.EmergencyBinding[annotationEmergencyInstanceID]) {
			return fmt.Errorf("emergency fallback source fence ownership mismatch")
		}
	}
	return nil
}
