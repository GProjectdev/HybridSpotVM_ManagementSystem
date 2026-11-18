package management

import (
	"context"
	"fmt"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A Verified world can resume checkpointing while its old provider terminates.
// Keep that separate from permission to allocate the next replacement: the
// provider finalizer must finish first. Never retire the source by name alone.
func (r *PolicyReconciler) pendingVerifiedGroupRetirement(ctx context.Context, input p.PolicyInput) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(newRestoreRequest().GroupVersionKind().GroupVersion().WithKind("RestoreRequestList"))
	if err := r.reader().List(ctx, list, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID), p.LabelRole: groupRestoreRole}); err != nil {
		return "", err
	}
	for i := range list.Items {
		req := &list.Items[i]
		if validateGroupVerified(req) != nil || stringField(req.Object, "spec", "groupRestore", "sourceWorldUID") != string(input.WorkloadRef.UID) {
			continue
		}
		pairs, err := groupReplacementPairs(req)
		if err != nil {
			return "", err
		}
		for _, pair := range pairs {
			old := p.NewObject("NodeProvision")
			if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: pair.OldName}, old); apierrors.IsNotFound(err) {
				continue
			} else if err != nil {
				return "", err
			}
			if string(old.GetUID()) != pair.OldUID || old.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) {
				return "", fmt.Errorf("verified group retirement source identity changed")
			}
			target := p.NewObject("NodeProvision")
			if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: pair.NewName}, target); err != nil {
				return "", err
			}
			if string(target.GetUID()) != pair.NewUID || target.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) || !target.GetDeletionTimestamp().IsZero() || stringField(target.Object, "status", "phase") != "Ready" {
				return "", fmt.Errorf("verified group retirement replacement identity/readiness changed")
			}
			return old.GetName(), nil
		}
	}
	return "", nil
}
