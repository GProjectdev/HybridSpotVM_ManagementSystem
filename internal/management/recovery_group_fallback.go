package management

import (
	"context"
	"fmt"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Replacement ownership uses the SpotReplacement name/UID; group fencing uses
// the old NodeProvision UID. Bind both before accepting group-only evidence.
func (r *RecoveryReconciler) verifyGroupFallbackCleanup(ctx context.Context, req *unstructured.Unstructured, spec recoverySpec) error {
	if !spec.sameClusterReplacement() || req.GetName() != spec.Operation+"-group-restore" || req.GetAnnotations()[groupCleanupOwner] != spec.Operation {
		return fmt.Errorf("group cleanup replacement owner mismatch")
	}
	owner := newSpotReplacementObject()
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: req.GetNamespace(), Name: spec.Operation}, owner); err != nil {
		return err
	}
	if owner.GetUID() == "" || req.GetAnnotations()[groupCleanupOwnerUID] != string(owner.GetUID()) || owner.GetLabels()[p.LabelPolicyUID] != spec.PolicyUID || !owner.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("group cleanup owner UID/policy mismatch")
	}
	replacement, err := readReplacementSpec(owner)
	if err != nil {
		return err
	}
	fallback, found, _ := unstructured.NestedMap(owner.Object, "status", "partialFallback")
	if !found || stringField(fallback, "phase") != partialFallbackHandoffRecorded {
		return fmt.Errorf("group cleanup requires recorded fallback handoff")
	}
	if err := verifyGroupFallbackRequest(req, replacement, fallback); err != nil {
		return err
	}
	if req.GetAnnotations()[groupOldName] != spec.OldNodeProvisionName || req.GetAnnotations()[groupOldUID] != spec.OldNodeProvisionUID || req.GetAnnotations()[groupNewName] != spec.ReplacementNodeProvisionName || req.GetAnnotations()[groupNewUID] != spec.ReplacementNodeProvisionUID {
		return fmt.Errorf("group cleanup node identities mismatch")
	}
	if stringField(req.Object, "spec", "groupRestore", "sourceWorldUID") != spec.WorkloadUID || boolField(req.Object, "spec", "sourceFenced") || !boolField(req.Object, "spec", "volumesReady") {
		return fmt.Errorf("group cleanup world/volume/fence intent mismatch")
	}
	if err := validateGroupVerified(req); err != nil {
		return err
	}
	sources, _, _ := unstructured.NestedSlice(req.Object, "spec", "groupRestore", "sourcePods")
	world := intField(req.Object, "spec", "groupRestore", "worldSize")
	if world <= 0 || int64(len(sources)) != world {
		return fmt.Errorf("group cleanup requires complete source identities")
	}
	seen := map[int64]bool{}
	oldFound := false
	for _, raw := range sources {
		source, ok := raw.(map[string]interface{})
		rank := intField(source, "rank")
		if !ok || rank < 0 || rank >= world || seen[rank] || stringField(source, "podName") == "" || stringField(source, "podUID") == "" || stringField(source, "nodeName") == "" || stringField(source, "nodeProvisionRef", "uid") == "" || stringField(source, "nodeProvisionRef", "instanceID") == "" {
			return fmt.Errorf("group cleanup invalid source identity")
		}
		seen[rank] = true
		oldFound = oldFound || stringField(source, "nodeProvisionRef", "name") == spec.OldNodeProvisionName
	}
	if !oldFound {
		return fmt.Errorf("group cleanup source does not include old NodeProvision")
	}
	return nil
}
