package framework

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A fence-only operation retains the NodeProvision receipt until RestorePlan has
// consumed it. Cleanup after the deadline uses the same already-pinned operation.
func authorizedFenceOperation(ctx context.Context, c client.Reader, cap, run *unstructured.Unstructured, a Allocation, entry map[string]interface{}, instance string) (string, error) {
	if a.FenceOperationUID == "" {
		if a.State == "Fence" {
			return "", fmt.Errorf("fence-only intent needs a transition UID")
		}
		return string(cap.GetUID()), nil
	}
	if prior, ok := entry["fenceOperationUID"].(string); ok {
		if prior != a.FenceOperationUID {
			return "", fmt.Errorf("fence operation changed")
		}
		return prior, nil
	}
	items, e := listItems(ctx, c, "training.dcnlab.com/v1alpha1", "TrainingTransition", run.GetNamespace())
	if e != nil {
		return "", e
	}
	for i := range items {
		t := &items[i]
		if string(t.GetUID()) != a.FenceOperationUID || !owned(t, run) {
			continue
		}
		var s TransitionSpec
		if decodeSpec(t, &s) != nil || s.CapacityRef.UID != string(cap.GetUID()) || (s.Mode != "Preemption" && s.Mode != "Failover") {
			return "", fmt.Errorf("unauthorized infrastructure fence")
		}
		matched := false
		for _, name := range s.RetireAllocations {
			if name == a.Name {
				matched = true
			}
		}
		if !matched {
			return "", fmt.Errorf("allocation not owned by transition retirement intent")
		}
		for _, source := range s.SourcePods {
			p := source.NodeProvisionRef
			if p != nil && p.Name == a.Name && p.UID == entry["memberUID"] && p.InstanceID == instance {
				return a.FenceOperationUID, nil
			}
		}
	}
	return "", fmt.Errorf("no immutable source provider identity authorizes this fence")
}
