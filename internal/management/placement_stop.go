package management

import (
	"context"
	"fmt"
	"reflect"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Preserve the member StatefulSet (and its identity) while stopping its Pods.
// A scheduler's empty or expanded zero-replica placement is not a migration.
func (h *PlacementAdmission) holdStoppedPlacement(ctx context.Context, policy, old, next *unstructured.Unstructured) (bool, error) {
	before, _, err := unstructured.NestedSlice(old.Object, "spec", "clusters")
	if err != nil {
		return false, err
	}
	after, _, err := unstructured.NestedSlice(next.Object, "spec", "clusters")
	if err != nil {
		return false, err
	}
	if len(before) != 1 || reflect.DeepEqual(before, after) {
		return false, nil
	}
	source, ok := before[0].(map[string]interface{})
	if !ok || stringField(source, "name") == "" {
		return false, fmt.Errorf("invalid source placement")
	}
	replicas, _, err := unstructured.NestedInt64(next.Object, "spec", "replicas")
	if err != nil {
		return false, err
	}
	if replicas != 0 {
		return false, nil
	}
	sts := p.NewObject("StatefulSet")
	name := stringField(next.Object, "spec", "resource", "name")
	uid := stringField(next.Object, "spec", "resource", "uid")
	if uid == "" || uid != stringField(old.Object, "spec", "resource", "uid") || name != stringField(old.Object, "spec", "resource", "name") {
		return false, fmt.Errorf("stop changed workload identity")
	}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: next.GetNamespace(), Name: name}, sts); err != nil {
		return false, fmt.Errorf("verify stopped workload: %w", err)
	}
	count, explicit, err := unstructured.NestedInt64(sts.Object, "spec", "replicas")
	if err != nil {
		return false, err
	}
	if !explicit || count != 0 {
		return false, nil
	}
	if string(sts.GetUID()) != uid || sts.GetDeletionTimestamp() != nil {
		return false, fmt.Errorf("stopped workload UID mismatch or deleting")
	}
	for _, object := range []*unstructured.Unstructured{old, next} {
		for _, key := range []string{pendingPlacementAnnotation, placementRequestAnnotation, placementRequestUIDAnnotation} {
			if object.GetAnnotations()[key] != "" {
				return false, fmt.Errorf("cannot stop while placement restore intent remains")
			}
		}
	}
	if policy.GetAnnotations()["training.dcnlab.com/group-restore-intent"] != "" {
		return false, fmt.Errorf("cannot stop while group restore intent remains")
	}
	plan := newRestoreRequest()
	plan.SetKind("RestorePlan")
	for _, prototype := range []*unstructured.Unstructured{newSpotReplacementObject(), newRestoreRequest(), plan, p.NewObject("FluidCRMigration")} {
		list := &unstructured.UnstructuredList{}
		gvk := prototype.GroupVersionKind()
		gvk.Kind += "List"
		list.SetGroupVersionKind(gvk)
		if err := h.Reader.List(ctx, list, client.InNamespace(next.GetNamespace())); err != nil {
			return false, fmt.Errorf("verify stop operations: %w", err)
		}
		for _, op := range list.Items {
			owned := (policy.GetUID() != "" && op.GetLabels()[p.LabelPolicyUID] == string(policy.GetUID())) || op.GetLabels()[p.LabelPolicy] == policy.GetName() || stringField(op.Object, "spec", "workloadRef", "uid") == uid || stringField(op.Object, "spec", "workloadRef", "name") == name
			if !owned {
				continue
			}
			if prototype.GetKind() == "FluidCRMigration" && op.GetDeletionTimestamp() == nil && isTerminalPhase(&op, stringField(source, "name")) {
				continue
			}
			return false, fmt.Errorf("cannot stop while %s %s remains", prototype.GetKind(), op.GetName())
		}
	}
	source["replicas"] = int64(0)
	return true, unstructured.SetNestedSlice(next.Object, before, "spec", "clusters")
}
