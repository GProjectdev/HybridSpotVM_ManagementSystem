package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reconstruct historical completion only from independent, UID-bound completed
// operation and verified restore receipts. Unlike the normal recovery path this
// performs no deletion; an existing (even deleting) old NodeProvision disallows it.
func (r *RecoveryReconciler) completedHistoricalCleanup(ctx context.Context, ns string, s recoverySpec) bool {
	if s.ReplacementNodeProvisionName == "" {
		return false
	}
	op := newSpotReplacementObject()
	if r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: s.Operation}, op) != nil ||
		!op.GetDeletionTimestamp().IsZero() || op.GetGeneration() < 1 ||
		stringField(op.Object, "status", "phase") != "Completed" ||
		intField(op.Object, "status", "observedGeneration") != op.GetGeneration() {
		return false
	}
	if r.verifySpotReplacement(ctx, ns, s) != nil {
		return false
	}
	if stringField(op.Object, "status", "replacementNodeProvisionRef", "uid") != s.ReplacementNodeProvisionUID ||
		stringField(op.Object, "status", "restoreRequestRef", "uid") != s.RestoreRequestUID {
		return false
	}
	old := p.NewObject("NodeProvision")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: s.OldNodeProvisionName}, old); !apierrors.IsNotFound(err) {
		return false
	}
	policy := p.NewObject("NodeRequest")
	if r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: s.PolicyName}, policy) != nil ||
		!policy.GetDeletionTimestamp().IsZero() || string(policy.GetUID()) != s.PolicyUID ||
		configuredAWSCluster(policy) != s.SourceCluster ||
		stringField(policy.Object, "spec", "workloadRef", "uid") != s.WorkloadUID {
		return false
	}
	replacement := p.NewObject("NodeProvision")
	if r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: s.ReplacementNodeProvisionName}, replacement) != nil ||
		!replacement.GetDeletionTimestamp().IsZero() || string(replacement.GetUID()) != s.ReplacementNodeProvisionUID ||
		replacement.GetLabels()[p.LabelPolicyUID] != s.PolicyUID {
		return false
	}
	if r.verifyRuntime(ctx, ns, s) != nil {
		return false
	}
	_, err := r.verifyRestoreRequest(ctx, ns, s)
	return err == nil
}
