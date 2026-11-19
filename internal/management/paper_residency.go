package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

// The v2 fixed-world protocol uses full-group recovery. Residency starts only
// at a UID-bound Verified restore onto a currently owned OD replacement.
// Retaining capacity never authorizes cleanup or relaxes a fencing gate.
func (r *PolicyReconciler) applyPaperResidency(ctx context.Context, input p.PolicyInput, runtime p.RuntimeSnapshot, risk p.RiskSnapshot, decision *p.Decision) error {
	if input.VMPolicyMethod != "paper-eq6-7" || input.FixedOnDemand != nil || input.WorkloadRef.UID == "" || decision.ProvisioningBlocked {
		return nil
	}
	seconds := input.Economics.OnDemandDurationSeconds
	if seconds <= 0 {
		seconds = 3600
	}
	evidenceInput := input
	evidenceInput.Economics.MaxAgeSeconds = seconds
	requests := &unstructured.UnstructuredList{}
	requests.SetGroupVersionKind(newRestoreRequest().GroupVersionKind().GroupVersion().WithKind("RestoreRequestList"))
	if err := r.reader().List(ctx, requests, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID), p.LabelRole: groupRestoreRole}); err != nil {
		return err
	}
	nodes := p.NewList("NodeProvision")
	if err := r.reader().List(ctx, nodes, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID)}); err != nil {
		return err
	}
	held := map[string]time.Time{}
	now := r.now()
	for i := range requests.Items {
		req := &requests.Items[i]
		if validateGroupVerified(req) != nil {
			continue
		}
		_, at, ok := verifiedRecoveryDuration(req, evidenceInput, now)
		if !ok || !now.Before(at.Add(time.Duration(seconds)*time.Second)) {
			continue
		}
		// Single-worker receipts retain their original compatibility contract.
		// A batch must retain every Verified OD destination, not only the
		// primary pair kept for legacy emergency handlers.
		targets := map[string]string{req.GetAnnotations()[groupNewName]: req.GetAnnotations()[groupNewUID]}
		if req.GetAnnotations()[groupBatchPairsAnnotation] != "" {
			pairs, err := groupReplacementPairs(req)
			if err != nil {
				return err
			}
			for _, pair := range pairs {
				targets[pair.NewName] = pair.NewUID
			}
		}
		for j := range nodes.Items {
			np := &nodes.Items[j]
			if np.GetUID() == "" || !np.GetDeletionTimestamp().IsZero() || targets[np.GetName()] != string(np.GetUID()) || stringField(np.Object, "spec", "marketType") != "OnDemand" || stringField(np.Object, "status", "phase") != "Ready" {
				continue
			}
			until := at.Add(time.Duration(seconds) * time.Second)
			if until.After(held[string(np.GetUID())]) {
				held[string(np.GetUID())] = until
			}
		}
	}
	count := input.MinOnDemand + int64(len(held))
	if count > input.TargetWorkers {
		count = input.TargetWorkers
	}
	if len(held) == 0 {
		return nil
	}
	var next time.Time
	for _, until := range held {
		if next.IsZero() || until.Before(next) {
			next = until
		}
	}
	decision.OnDemandHoldUntil = next.Format(time.RFC3339Nano)
	if count <= decision.OnDemandWorkers {
		return nil
	}
	decision.OnDemandWorkers, decision.SpotWorkers = count, input.TargetWorkers-count
	decision.Reason = "paper_on_demand_minimum_residency"
	decision.CheckpointIntervalSeconds, decision.IntervalCostEvaluated = p.AdaptiveCheckpointInterval(input.Checkpoint, risk, runtime, decision.SpotWorkers, now)
	return nil
}
