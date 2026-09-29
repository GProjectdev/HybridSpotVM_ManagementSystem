package management

import (
	"context"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCoordinatorRejectsConcurrentReplacementOperations(t *testing.T) {
	first := replacementOperationFixture()
	first.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: "policy-uid"})
	second := first.DeepCopy()
	second.SetName("second")
	second.SetUID("second-uid")
	r := replacementReconcilerFixture(t, time.Now(), first, second)
	cp := &CheckpointReconciler{Client: r.Client}
	active, err := cp.activeSpotReplacement(context.Background(), trainingpolicy.PolicyInput{Namespace: "default", PolicyUID: "policy-uid"})
	if err == nil || active != nil {
		t.Fatal("ambiguous active operations accepted")
	}
}

func TestCheckpointRejectsDifferentAttemptOrIntent(t *testing.T) {
	spec, err := readReplacementSpec(replacementOperationFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "new-attempt", "missing-attempt", "workload", "ranks", "resume"} {
		t.Run(mode, func(t *testing.T) {
			cp := replacementPartialCheckpointFixture(true)
			annotations := cp.GetAnnotations()
			switch mode {
			case "new-attempt":
				annotations["training.dcnlab.com/recovery-operation-uid"] = "old-attempt"
			case "missing-attempt":
				delete(annotations, "training.dcnlab.com/recovery-operation-uid")
			case "workload":
				_ = unstructured.SetNestedField(cp.Object, "another-workload", "spec", "workloadRef", "uid")
			case "ranks":
				_ = unstructured.SetNestedSlice(cp.Object, []interface{}{int64(0)}, "spec", "partialCheckpoint", "targetRanks")
			case "resume":
				_ = unstructured.SetNestedField(cp.Object, true, "spec", "resume")
			}
			cp.SetAnnotations(annotations)
			err := verifyReplacementCheckpointIdentity(cp, spec)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("identity check: %v", err)
			}
		})
	}
}
