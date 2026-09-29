package management

import (
	"context"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Calibration is opt-in. It never certifies restoration or authorizes deletion.
// Request-to-verification latency is an end-to-end recovery proxy, not CRIU time.
func (r *PolicyReconciler) applyRecoveryEconomics(ctx context.Context, input *p.PolicyInput, runtime p.RuntimeSnapshot, risk p.RiskSnapshot) error {
	if !input.Economics.Enabled || input.Economics.LossCostPerEviction >= 0 {
		return nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("migration.dcnlab.com/v1alpha1")
	list.SetKind("RestoreRequestList")
	if err := r.List(ctx, list, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID)}); err != nil {
		return err
	}
	var latest time.Time
	var seconds float64
	for i := range list.Items {
		d, at, ok := verifiedRecoveryDuration(&list.Items[i], *input, r.now())
		if ok && at.After(latest) {
			seconds, latest = d, at
		}
	}
	if latest.IsZero() || !positiveFinite(risk.SpotPricePerHour) || !positiveFinite(risk.OnDemandPricePerHour) {
		return nil
	}
	interval, evaluated := p.AdaptiveCheckpointInterval(input.Checkpoint, risk, runtime, 1, r.now())
	if !evaluated || interval <= 0 {
		// A bootstrap interval is not evidence of lost work.
		return nil
	}
	// Uniform interruption within a checkpoint interval: expected lost work = f/2.
	input.Economics.LossCostPerEviction = float64(interval)/2/3600*risk.SpotPricePerHour + seconds/3600*risk.OnDemandPricePerHour
	input.Economics.ObservedAt = latest.Format(time.RFC3339Nano)
	input.Economics.Source = "verified-request-latency-and-half-interval"
	return nil
}

func verifiedRecoveryDuration(req *unstructured.Unstructured, input p.PolicyInput, now time.Time) (float64, time.Time, bool) {
	zero := time.Time{}
	if !req.GetDeletionTimestamp().IsZero() || req.GetUID() == "" || req.GetGeneration() < 1 ||
		req.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) ||
		stringField(req.Object, "spec", "workloadRef", "uid") != string(input.WorkloadRef.UID) ||
		input.WorkloadRef.UID == "" || stringField(req.Object, "status", "phase") != "Verified" ||
		intField(req.Object, "status", "observedGeneration") != req.GetGeneration() ||
		stringField(req.Object, "status", "verification", "requestUID") != string(req.GetUID()) {
		return 0, zero, false
	}
	checkpoint := stringField(req.Object, "spec", "checkpointRef", "checkpointID")
	operation := stringField(req.Object, "spec", "groupRestore", "operationUID")
	if operation == "" {
		operation = req.GetAnnotations()["training.dcnlab.com/recovery-operation"]
	}
	if operation == "" || stringField(req.Object, "status", "verification", "operation") != operation {
		return 0, zero, false
	}
	if checkpoint == "" || stringField(req.Object, "status", "verification", "checkpointID") != checkpoint ||
		stringField(req.Object, "spec", "trainingRuntimeRef", "name") != input.RuntimeRefName {
		return 0, zero, false
	}
	for _, field := range []string{"name", "uid"} {
		value := stringField(req.Object, "spec", "trainingRuntimeRef", field)
		if value == "" || stringField(req.Object, "status", "verification", "trainingRuntimeRef", field) != value {
			return 0, zero, false
		}
	}
	for _, field := range []string{"sourceCluster", "targetCluster"} {
		value := stringField(req.Object, "spec", field)
		if value == "" || stringField(req.Object, "status", "verification", field) != value {
			return 0, zero, false
		}
	}
	if stringField(req.Object, "spec", "targetCluster") != input.SourceCluster {
		return 0, zero, false
	}
	at, err := time.Parse(time.RFC3339Nano, stringField(req.Object, "status", "verification", "verifiedAt"))
	maxAge := input.Economics.MaxAgeSeconds
	if maxAge <= 0 {
		maxAge = 600
	}
	start := req.GetCreationTimestamp().Time
	if err != nil || start.IsZero() || !at.After(start) || at.After(now) || now.Sub(at).Seconds() > float64(maxAge) {
		return 0, zero, false
	}
	return at.Sub(start).Seconds(), at, true
}
