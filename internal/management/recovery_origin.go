package management

import (
	"context"
	"fmt"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A replacement keeps its creation operation annotation. A subsequent retirement
// must prove that origin instead of requiring the annotation to change owners.
func (r *RecoveryReconciler) verifyOldNodeOrigin(ctx context.Context, np *unstructured.Unstructured, s recoverySpec, restore restoreEvidence) error {
	origin := np.GetAnnotations()["training.dcnlab.com/recovery-operation"]
	if origin == "" || origin == s.Operation {
		return verifyOldNodeRIC(np, s, restore)
	}
	op := newSpotReplacementObject()
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: np.GetNamespace(), Name: origin}, op); err != nil {
		return fmt.Errorf("get old NodeProvision creation receipt: %w", err)
	}
	if !op.GetDeletionTimestamp().IsZero() || op.GetGeneration() < 1 ||
		stringField(op.Object, "status", "phase") != "Completed" ||
		intField(op.Object, "status", "observedGeneration") != op.GetGeneration() ||
		stringField(op.Object, "spec", "operation") != origin ||
		stringField(op.Object, "spec", "policyRef", "uid") != s.PolicyUID ||
		stringField(op.Object, "spec", "workloadRef", "uid") != s.WorkloadUID ||
		stringField(op.Object, "spec", "targetCluster") != s.SourceCluster ||
		stringField(op.Object, "spec", "replacementNodeProvisionRef", "name") != np.GetName() ||
		stringField(op.Object, "status", "replacementNodeProvisionRef", "name") != np.GetName() ||
		stringField(op.Object, "status", "replacementNodeProvisionRef", "uid") != string(np.GetUID()) {
		return fmt.Errorf("old NodeProvision creation receipt does not match UID, policy, workload and cluster")
	}
	// A previously verified restore is historical evidence, not proof that the
	// new world still runs. In particular, do not retire the OD fallback when a
	// newly restored Spot has already been interrupted.
	if err := r.verifyCurrentRetirementRuntime(ctx, np.GetNamespace(), s); err != nil {
		return err
	}
	copy := np.DeepCopy()
	annotations := copy.GetAnnotations()
	annotations["training.dcnlab.com/recovery-operation"] = s.Operation
	copy.SetAnnotations(annotations)
	return verifyOldNodeRIC(copy, s, restore)
}

func (r *RecoveryReconciler) verifyCurrentRetirementRuntime(ctx context.Context, ns string, s recoverySpec) error {
	rt := p.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: ns, Name: s.TrainingRuntimeName}, rt); err != nil {
		return err
	}
	if string(rt.GetUID()) != s.TrainingRuntimeUID || !rt.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("retirement runtime UID changed or is deleting")
	}
	reports, _, _ := unstructured.NestedSlice(rt.Object, "status", "clusters")
	for _, item := range reports {
		report, ok := item.(map[string]interface{})
		if !ok || stringField(report, "clusterName") != s.TargetCluster {
			continue
		}
		status, _, _ := unstructured.NestedMap(report, "status")
		observed, err := time.Parse(time.RFC3339Nano, stringField(status, "observedAt"))
		age := r.now().Sub(observed)
		expected := intField(rt.Object, "spec", "expectedWorldSize")
		if err == nil && age >= 0 && age <= 2*time.Minute && expected > 0 &&
			intField(status, "observedGeneration") == rt.GetGeneration() &&
			stringField(status, "sourceWorldUID") == s.WorkloadUID &&
			stringField(status, "phase") == "Running" && intField(status, "readyRanks") == expected {
			return nil
		}
	}
	return fmt.Errorf("current UID-bound running world required before retiring a previous replacement")
}
