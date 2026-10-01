package management

import (
	"context"
	"fmt"
	"reflect"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const checkpointScheduleRole = "checkpoint-schedule"
const checkpointEvidenceRole = "checkpoint-evidence"

func isCheckpointSchedule(obj *unstructured.Unstructured) bool {
	_, found, _ := unstructured.NestedMap(obj.Object, "spec", "schedule")
	return found
}

func (r *CheckpointReconciler) ensureCheckpointSchedule(ctx context.Context, input p.PolicyInput, runtime p.RuntimeSnapshot, interval int64) (*unstructured.Unstructured, error) {
	if !input.Checkpoint.Resume || interval <= 0 {
		return nil, fmt.Errorf("periodic checkpoint requires resume=true and a positive interval")
	}
	desired := p.NewFluidCRMigration(input, runtime, r.now(), interval)
	desired.SetName(input.PolicyName + "-periodic")
	labels := desired.GetLabels()
	labels[p.LabelRole] = checkpointScheduleRole
	desired.SetLabels(labels)
	desired.SetAnnotations(nil)
	desired.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "training.dcnlab.com/v1alpha1", Kind: "TrainingPolicy", Name: input.PolicyName, UID: input.PolicyUID}})
	_ = unstructured.SetNestedMap(desired.Object, map[string]interface{}{"enabled": true, "intervalSeconds": interval}, "spec", "schedule")
	current := p.NewObject("FluidCRMigration")
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		current = desired
	} else if err != nil {
		return nil, err
	} else {
		if !current.GetDeletionTimestamp().IsZero() || current.GetLabels()[p.LabelRole] != checkpointScheduleRole || current.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) || !isCheckpointSchedule(current) || stringField(current.Object, "spec", "workloadRef", "uid") != string(input.WorkloadRef.UID) {
			return nil, fmt.Errorf("periodic checkpoint ownership mismatch")
		}
		if intField(current.Object, "spec", "ctrlPort") != runtime.Port || stringField(current.Object, "spec", "container") != runtime.Container {
			if boolField(current.Object, "spec", "schedule", "enabled") {
				before := current.DeepCopy()
				_ = unstructured.SetNestedField(current.Object, false, "spec", "schedule", "enabled")
				if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					return nil, err
				}
			}
			return nil, fmt.Errorf("periodic checkpoint paused: immutable runtime port/container changed; replace the schedule only after its active round is quiesced")
		}
		before := current.DeepCopy()
		current.SetOwnerReferences(desired.GetOwnerReferences())
		_ = unstructured.SetNestedMap(current.Object, map[string]interface{}{"enabled": true, "intervalSeconds": interval}, "spec", "schedule")
		if !reflect.DeepEqual(before.Object["spec"], current.Object["spec"]) || !reflect.DeepEqual(before.GetOwnerReferences(), current.GetOwnerReferences()) {
			if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return nil, err
			}
		}
	}
	return current, r.createIfMissing(ctx, p.NewPropagationPolicyFor(input, current, input.SourceCluster))
}

// Require a member acknowledgement before starting a competing operation.
func (r *CheckpointReconciler) pauseCheckpointSchedules(ctx context.Context, input p.PolicyInput) (bool, error) {
	list := p.NewList("FluidCRMigration")
	if err := r.List(ctx, list, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID), p.LabelRole: checkpointScheduleRole}); err != nil {
		return false, err
	}
	ready := true
	for i := range list.Items {
		obj := &list.Items[i]
		if boolField(obj.Object, "spec", "schedule", "enabled") {
			before := obj.DeepCopy()
			_ = unstructured.SetNestedField(obj.Object, false, "spec", "schedule", "enabled")
			if err := r.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, err
			}
			ready = false
		}
		report, ok := sourceClusterStatus(obj, input.SourceCluster)
		if !ok || intField(report, "observedGeneration") != obj.GetGeneration() || stringField(report, "phase") != "Paused" {
			ready = false
		}
	}
	return ready, nil
}

func (r *CheckpointReconciler) scheduleMustPause(ctx context.Context, policy *unstructured.Unstructured, input p.PolicyInput) (bool, error) {
	if input.Suspended || !input.Checkpoint.Resume || policy.GetAnnotations()[groupIntentAnnotation] != "" || !discoveryReady(policy) {
		return true, nil
	}
	if validatePolicyInput(input) != nil || r.validateLiveWorkloadUID(ctx, input) != nil {
		return true, nil
	}
	group, err := activeGroupRestore(ctx, r.Client, input)
	if err != nil || group != nil {
		return true, err
	}
	op, err := r.activeSpotReplacement(ctx, input)
	if err != nil || op != nil {
		return true, err
	}
	event, err := r.unhandledEmergencySpotEvent(ctx, input)
	if err != nil || event != nil {
		return true, err
	}
	risk, err := r.riskSnapshot(ctx, input)
	if err != nil {
		return true, err
	}
	if !p.RiskFreshForPolicy(input, risk, r.now()) {
		return true, nil
	}
	return false, nil
}

// Retained records mirror actual member evidence; they must never be propagated.
func (r *CheckpointReconciler) syncScheduledEvidence(ctx context.Context, input p.PolicyInput) error {
	list := p.NewList("FluidCRMigration")
	if err := r.List(ctx, list, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID), p.LabelRole: checkpointScheduleRole}); err != nil {
		return err
	}
	for i := range list.Items {
		parent := &list.Items[i]
		report, ok := sourceClusterStatus(parent, input.SourceCluster)
		if !ok {
			continue
		}
		ref, found, err := unstructured.NestedMap(report, "lastSuccessfulFullCheckpoint")
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if err := r.mirrorScheduledRound(ctx, input, parent, ref); err != nil {
			return err
		}
	}
	return nil
}

func (r *CheckpointReconciler) mirrorScheduledRound(ctx context.Context, input p.PolicyInput, parent *unstructured.Unstructured, ref map[string]interface{}) error {
	name, uid, id := stringField(ref, "name"), stringField(ref, "uid"), stringField(ref, "checkpointID")
	if name == "" || uid == "" || id == "" || name == parent.GetName() {
		return fmt.Errorf("invalid scheduled round identity")
	}
	spec, found, err := unstructured.NestedMap(ref, "result", "spec")
	if err != nil || !found {
		return fmt.Errorf("scheduled checkpoint spec missing")
	}
	report, found, err := unstructured.NestedMap(ref, "result", "status")
	if err != nil || !found {
		return fmt.Errorf("scheduled checkpoint evidence missing")
	}
	if _, exists := spec["schedule"]; exists {
		return fmt.Errorf("nested checkpoint schedule is not a result")
	}
	obj := p.NewObject("FluidCRMigration")
	obj.SetNamespace(input.Namespace)
	obj.SetName(name)
	obj.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID), p.LabelPolicy: input.PolicyName, p.LabelManagedBy: "hybridspotvm-system", p.LabelRole: checkpointEvidenceRole})
	obj.SetAnnotations(map[string]string{"training.dcnlab.com/checkpoint-id": id, "training.dcnlab.com/source-checkpoint-uid": uid, "training.dcnlab.com/schedule-uid": string(parent.GetUID()), "training.dcnlab.com/started-at": stringField(report, "startTime")})
	obj.Object["spec"] = spec
	// Validate the source snapshot before creating a control-plane evidence object.
	obj.SetUID("validation-only")
	obj.SetGeneration(1)
	report["clusterName"] = input.SourceCluster
	obj.Object["status"] = map[string]interface{}{"clusters": []interface{}{report}}
	if _, err := readGroupCheckpoint(obj, input); err != nil {
		return fmt.Errorf("scheduled round rejected: %w", err)
	}
	current := p.NewObject("FluidCRMigration")
	err = r.Get(ctx, client.ObjectKeyFromObject(obj), current)
	if apierrors.IsNotFound(err) {
		obj.SetUID("")
		obj.SetGeneration(0)
		delete(obj.Object, "status")
		if err := r.Create(ctx, obj); err != nil {
			return err
		}
		current = obj
	} else if err != nil {
		return err
	}
	if current.GetLabels()[p.LabelRole] != checkpointEvidenceRole || current.GetAnnotations()["training.dcnlab.com/source-checkpoint-uid"] != uid || current.GetAnnotations()["training.dcnlab.com/schedule-uid"] != string(parent.GetUID()) || !reflect.DeepEqual(current.Object["spec"], spec) {
		return fmt.Errorf("scheduled result name collision")
	}
	want := map[string]interface{}{"clusters": []interface{}{report}}
	if reflect.DeepEqual(current.Object["status"], want) {
		return nil
	}
	if existing, found, _ := unstructured.NestedSlice(current.Object, "status", "clusters"); found && len(existing) != 0 {
		return fmt.Errorf("retained checkpoint evidence changed")
	}
	before := current.DeepCopy()
	current.Object["status"] = want
	return r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
