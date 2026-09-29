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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Coordinator serialization prevents periodic and replacement requests racing.
func (r *CheckpointReconciler) coordinateReplacementCheckpoint(ctx context.Context, op *unstructured.Unstructured) error {
	spec, err := readReplacementSpec(op)
	if err != nil {
		return err
	}
	if !op.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("replacement is deleting")
	}
	if spec.OperationUID == "" {
		return fmt.Errorf("replacement UID is required before checkpoint creation")
	}
	verifier := &ReplacementReconciler{Client: r.Client, APIReader: r.Client, Clock: r.Clock}
	if err := verifier.verifyReplacementPolicy(ctx, op.GetNamespace(), spec); err != nil {
		return err
	}
	// Repair placement after fencing without repeating checkpointing.
	existing := trainingpolicy.NewObject("FluidCRMigration")
	err = r.Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: spec.Operation + "-partial-checkpoint"}, existing)
	if err == nil {
		_, _, err = r.ensurePartialCheckpoint(ctx, op.GetNamespace(), spec)
		return err
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	old := trainingpolicy.NewObject("NodeProvision")
	if err := r.Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: spec.OldNodeProvisionName}, old); err != nil {
		return err
	}
	if err := verifyReplacementOldNode(old, spec); err != nil {
		return err
	}
	if spec.EmergencyEventID == "" {
		replacement := trainingpolicy.NewObject("NodeProvision")
		if err := r.Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: spec.ReplacementNodeProvisionName}, replacement); err != nil {
			return err
		}
		if err := verifyReplacementReadyForOperation(replacement, spec); err != nil {
			return err
		}
	}
	input := trainingpolicy.PolicyInput{Namespace: op.GetNamespace(), PolicyUID: types.UID(spec.PolicyUID), SourceCluster: spec.SourceCluster}
	_, inflight, _, _, err := r.checkpointState(ctx, input)
	if err != nil {
		return err
	}
	if inflight {
		return fmt.Errorf("waiting for periodic checkpoint before replacement checkpoint")
	}
	_, _, err = r.ensurePartialCheckpoint(ctx, op.GetNamespace(), spec)
	return err
}

func (r *CheckpointReconciler) ensurePartialCheckpoint(ctx context.Context, ns string, spec replacementSpec) (*unstructured.Unstructured, bool, error) {
	name := spec.Operation + "-partial-checkpoint"
	existing := trainingpolicy.NewObject("FluidCRMigration")
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, existing)
	if err == nil {
		if err := verifyReplacementCheckpointIdentity(existing, spec); err != nil {
			return nil, false, err
		}
		input := trainingpolicy.PolicyInput{Namespace: ns, PolicyName: spec.PolicyName, PolicyUID: types.UID(spec.PolicyUID)}
		if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, existing, spec.SourceCluster)); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	desired := trainingpolicy.NewObject("FluidCRMigration")
	desired.SetNamespace(ns)
	desired.SetName(name)
	desired.SetLabels(map[string]string{
		trainingpolicy.LabelManagedBy: "hybridspotvm-system",
		trainingpolicy.LabelPolicy:    spec.PolicyName,
		trainingpolicy.LabelPolicyUID: spec.PolicyUID,
		trainingpolicy.LabelRole:      "replacement-checkpoint",
	})
	desired.SetAnnotations(map[string]string{
		"training.dcnlab.com/recovery-operation":     spec.Operation,
		"training.dcnlab.com/recovery-operation-uid": spec.OperationUID,
		"training.dcnlab.com/started-at":             r.now().UTC().Format(time.RFC3339),
		"training.dcnlab.com/checkpoint-id":          name,
	})
	desired.Object["spec"] = map[string]interface{}{
		"workloadRef": map[string]interface{}{
			"apiVersion": spec.WorkloadAPIVersion,
			"kind":       spec.WorkloadKind,
			"name":       spec.WorkloadName,
			"uid":        spec.WorkloadUID,
		},
		"resume": false,
		"partialCheckpoint": map[string]interface{}{
			"targetRanks": int64SliceToInterface(spec.TargetRanks),
		},
	}
	if err := r.Create(ctx, desired); err != nil {
		return nil, false, err
	}
	input := trainingpolicy.PolicyInput{Namespace: ns, PolicyName: spec.PolicyName, PolicyUID: types.UID(spec.PolicyUID)}
	if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, desired, spec.SourceCluster)); err != nil {
		return nil, false, err
	}
	return desired, true, nil
}

func verifyReplacementCheckpointIdentity(cp *unstructured.Unstructured, spec replacementSpec) error {
	if !cp.GetDeletionTimestamp().IsZero() ||
		cp.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID ||
		cp.GetLabels()[trainingpolicy.LabelRole] != "replacement-checkpoint" ||
		spec.OperationUID == "" ||
		cp.GetAnnotations()["training.dcnlab.com/recovery-operation-uid"] != spec.OperationUID ||
		cp.GetAnnotations()["training.dcnlab.com/recovery-operation"] != spec.Operation {
		return fmt.Errorf("existing partial checkpoint ownership or attempt UID mismatch")
	}
	ref, _, _ := unstructured.NestedMap(cp.Object, "spec", "workloadRef")
	want := map[string]interface{}{"apiVersion": spec.WorkloadAPIVersion, "kind": spec.WorkloadKind, "name": spec.WorkloadName, "uid": spec.WorkloadUID}
	ranks, _, _ := unstructured.NestedSlice(cp.Object, "spec", "partialCheckpoint", "targetRanks")
	resume, found, err := unstructured.NestedBool(cp.Object, "spec", "resume")
	if !reflect.DeepEqual(ref, want) || !reflect.DeepEqual(ranks, int64SliceToInterface(spec.TargetRanks)) || !found || err != nil || resume {
		return fmt.Errorf("existing partial checkpoint workload, target ranks or resume intent mismatch")
	}
	return nil
}
