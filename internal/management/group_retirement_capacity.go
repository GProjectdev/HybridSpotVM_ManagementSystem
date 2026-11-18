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
		a := req.GetAnnotations()
		if a[groupOldName] == "" {
			continue
		}
		if a[groupOldUID] == "" || a[groupNewName] == "" || a[groupNewUID] == "" {
			return "", fmt.Errorf("verified group retirement has incomplete provider identities")
		}
		old := p.NewObject("NodeProvision")
		if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: a[groupOldName]}, old); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return "", err
		}
		if string(old.GetUID()) != a[groupOldUID] || old.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) {
			return "", fmt.Errorf("verified group retirement source identity changed")
		}
		replacement := p.NewObject("NodeProvision")
		if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: a[groupNewName]}, replacement); err != nil {
			return "", err
		}
		if string(replacement.GetUID()) != a[groupNewUID] || replacement.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) || !replacement.GetDeletionTimestamp().IsZero() || stringField(replacement.Object, "status", "phase") != "Ready" {
			return "", fmt.Errorf("verified group retirement replacement identity/readiness changed")
		}
		return old.GetName(), nil
	}
	return "", nil
}
