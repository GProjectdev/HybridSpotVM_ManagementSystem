package management

import (
	"context"
	"fmt"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// An existing partial operation retains ownership even when runtime samples
// become stale. Never start a second actuator for the same source world.
func (r *PolicyReconciler) ensurePlannedPartialReplacement(ctx context.Context, policy *unstructured.Unstructured, input p.PolicyInput, old *unstructured.Unstructured, operation, replacement, market, emergencyEventID string) (bool, bool, error) {
	existing := newSpotReplacementObject()
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: operation}, existing)
	if err == nil {
		if emergencyEventID != "" && existing.GetAnnotations()[annotationEmergencyEventID] != emergencyEventID {
			return true, false, fmt.Errorf("existing partial operation requires verified interruption ownership handoff")
		}
		if stringField(existing.Object, "spec", "policyRef", "uid") != string(input.PolicyUID) || stringField(existing.Object, "spec", "oldNodeProvisionRef", "uid") != string(old.GetUID()) || stringField(existing.Object, "spec", "desiredMarketType") != market {
			return true, false, fmt.Errorf("existing partial replacement identity mismatch")
		}
		return true, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return true, false, err
	}
	if policy.GetAnnotations()[groupIntentAnnotation] != "" || input.SourceCluster != "aws" || input.Capacity.AWSCluster != "aws" {
		return false, false, nil
	}
	if active, err := activeGroupRestore(ctx, r.reader(), input); err != nil {
		return true, false, err
	} else if active != nil {
		return false, false, nil
	}
	if _, found, _ := unstructured.NestedMap(old.Object, "spec", "fence"); found {
		return false, false, nil
	}
	if _, found, _ := unstructured.NestedMap(old.Object, "status", "fence"); found {
		return false, false, nil
	}
	if !old.GetDeletionTimestamp().IsZero() || stringField(old.Object, "status", "phase") != "Ready" {
		return false, false, nil
	}
	if emergencyEventID != "" {
		if err := verifyReplacementOldNode(old, replacementSpec{Operation: operation, PolicyUID: string(input.PolicyUID), OldNodeProvisionName: old.GetName(), OldNodeProvisionUID: string(old.GetUID()), DesiredMarketType: market, EmergencyEventID: emergencyEventID}); err != nil {
			return true, false, err
		}
	} else if boolField(old.Object, "status", "spot", "atRisk") {
		return false, false, nil
	}
	runtime := p.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.RuntimeRefName}, runtime); err != nil {
		return false, false, err
	}
	health := p.ReadRuntimeStatus(runtime, input.SourceCluster)
	if !p.RuntimeReadyForCheckpoint(input, health, r.now()) {
		return false, false, nil
	}
	sources, err := r.groupSourcePods(ctx, input)
	if err != nil {
		return false, false, err
	}
	var targets, survivors, ranks []interface{}
	for _, raw := range sources {
		s := raw.(map[string]interface{})
		matched := false
		for _, pod := range health.Pods {
			if pod.Rank == intField(s, "rank") && pod.Name == stringField(s, "podName") && pod.UID == stringField(s, "podUID") {
				matched = true
			}
		}
		if !matched {
			return true, false, fmt.Errorf("partial source snapshot differs from fresh runtime for %s", stringField(s, "podName"))
		}
		if stringField(s, "nodeName") == stringField(old.Object, "status", "nodeName") {
			ranks = append(ranks, s["rank"])
			targets = append(targets, map[string]interface{}{"rank": s["rank"], "sourcePod": s["podName"], "sourcePodUID": s["podUID"], "sourceNode": s["nodeName"]})
		} else {
			survivors = append(survivors, map[string]interface{}{"rank": s["rank"], "podName": s["podName"], "podUID": s["podUID"], "nodeName": s["nodeName"]})
		}
	}
	if len(targets) == 0 || len(survivors) == 0 {
		return false, false, nil
	}
	if _, _, err := r.groupCheckpointVolume(ctx, input); err != nil {
		return true, false, err
	}
	active, err := (&CheckpointReconciler{Client: r.Client}).activeSpotReplacement(ctx, input)
	if err != nil {
		return true, false, err
	}
	if active != nil {
		return true, false, fmt.Errorf("partial replacement %s already owns the workload", active.GetName())
	}
	op := newSpotReplacementObject()
	op.SetNamespace(input.Namespace)
	op.SetName(operation)
	if emergencyEventID != "" {
		op.SetAnnotations(map[string]string{annotationEmergencyEventID: emergencyEventID})
	}
	op.SetLabels(map[string]string{p.LabelManagedBy: "hybridspotvm-system", p.LabelPolicy: input.PolicyName, p.LabelPolicyUID: string(input.PolicyUID), p.LabelRole: "replacement-operation"})
	op.Object["spec"] = map[string]interface{}{
		"operation":     operation,
		"policyRef":     map[string]interface{}{"name": input.PolicyName, "uid": string(input.PolicyUID), "generation": input.Generation},
		"workloadRef":   map[string]interface{}{"apiVersion": input.WorkloadRef.APIVersion, "kind": input.WorkloadRef.Kind, "name": input.WorkloadRef.Name, "uid": string(input.WorkloadRef.UID)},
		"sourceCluster": input.SourceCluster, "targetCluster": input.SourceCluster,
		"oldNodeProvisionRef":         map[string]interface{}{"name": old.GetName(), "uid": string(old.GetUID())},
		"replacementNodeProvisionRef": map[string]interface{}{"name": replacement},
		"desiredMarketType":           market, "pods": targets,
		"partialCheckpoint": map[string]interface{}{"targetRanks": ranks},
		"partialRestore":    map[string]interface{}{"preventPeriodicResume": true, "targetRanks": ranks, "preservedSurvivors": survivors},
	}
	if _, err := readReplacementSpec(op); err != nil {
		return true, false, err
	}
	if err := r.Create(ctx, op); err != nil {
		return true, false, err
	}
	return true, true, nil
}
