package management

import (
	"context"
	"fmt"
	"sort"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Recovery deduplication is independent of periodic checkpoint event accounting.
func (r *CheckpointReconciler) ensureEmergencyReplacement(ctx context.Context, policy *unstructured.Unstructured, input trainingpolicy.PolicyInput) (string, error) {
	if !boolField(policy.Object, "spec", "replacement", "enabled") {
		return "", nil
	}
	active, err := r.activeSpotReplacement(ctx, input)
	if err != nil {
		return "", err
	}
	nodes := trainingpolicy.NewList("NodeProvision")
	if err := r.List(ctx, nodes, client.InNamespace(input.Namespace), client.MatchingLabels{trainingpolicy.LabelPolicyUID: string(input.PolicyUID)}); err != nil {
		return "", err
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].GetName() < nodes.Items[j].GetName() })
	for i := range nodes.Items {
		node := &nodes.Items[i]
		event, ok := readEmergencySpotEvent(node, input)
		if !ok {
			continue
		}
		if !validSpotSignalType(stringField(node.Object, "status", "spot", "signalType")) {
			return "", fmt.Errorf("emergency replacement requires an interruption or rebalance signal")
		}
		if active != nil {
			if active.GetAnnotations()[annotationEmergencyEventID] == event.EventID && stringField(active.Object, "spec", "oldNodeProvisionRef", "uid") == event.NodeUID {
				return active.GetName(), nil
			}
			return active.GetName(), fmt.Errorf("partial operation %s owns this world; interruption %s requires verified actuator handoff before group recovery", active.GetName(), event.EventID)
		}
		if input.SourceCluster != input.Capacity.AWSCluster || event.NodeUID == "" || !node.GetDeletionTimestamp().IsZero() || stringField(node.Object, "spec", "marketType") != "Spot" {
			return "", fmt.Errorf("emergency recovery requires a live UID-bound Spot NodeProvision in the AWS source cluster")
		}
		name := replacementOperationName(node.GetName(), event.NodeUID)
		existing := newSpotReplacementObject()
		err := r.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: name}, existing)
		if err == nil {
			if stringField(existing.Object, "spec", "policyRef", "uid") != string(input.PolicyUID) || stringField(existing.Object, "spec", "oldNodeProvisionRef", "uid") != event.NodeUID {
				return "", fmt.Errorf("emergency replacement name collision: %s", name)
			}
			if stringField(existing.Object, "status", "phase") == "Completed" {
				continue
			}
			return name, nil
		}
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		snapshot, err := r.runtimeSnapshot(ctx, input)
		if err != nil {
			return "", err
		}
		if !trainingpolicy.RuntimeReadyForCheckpoint(input, snapshot, r.now()) {
			producer := &PolicyReconciler{Client: r.Client, APIReader: r.Client, Clock: r.Clock}
			_, err := producer.ensureGroupReplacement(ctx, policy, input, node, name, replacementNodeProvisionName(node.GetName(), event.NodeUID), "OnDemand", event.EventID)
			return name, err
		}
		if err := r.validateLiveWorkloadUID(ctx, input); err != nil {
			return "", err
		}
		producer := &PolicyReconciler{Client: r.Client, APIReader: r.Client, Clock: r.Clock}
		if _, err := producer.ensureAutomaticSpotReplacement(ctx, policy, input, node, name, replacementNodeProvisionName(node.GetName(), event.NodeUID), "OnDemand", event.EventID); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", nil
}
