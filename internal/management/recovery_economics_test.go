package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRecoveryEconomicsRequiresVerifiedIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	input := p.PolicyInput{Namespace: "default", PolicyUID: "policy", SourceCluster: "aws", RuntimeRefName: "runtime", WorkloadRef: p.WorkloadRef{UID: "workload"}, Economics: p.EconomicsPolicy{Enabled: true, LossCostPerEviction: -1}}
	req := newRestoreRequest()
	req.SetName("restore")
	req.SetNamespace("default")
	req.SetUID("request")
	req.SetGeneration(2)
	req.SetCreationTimestamp(metav1.NewTime(now.Add(-time.Minute)))
	req.SetLabels(map[string]string{p.LabelPolicyUID: "policy"})
	req.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": "op"})
	req.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload"}, "checkpointRef": map[string]interface{}{"checkpointID": "ckpt"}, "sourceCluster": "aws", "targetCluster": "aws", "trainingRuntimeRef": map[string]interface{}{"name": "runtime", "uid": "runtime-uid"}}
	req.Object["status"] = map[string]interface{}{"phase": "Verified", "observedGeneration": int64(2), "verification": map[string]interface{}{"requestUID": "request", "operation": "op", "checkpointID": "ckpt", "verifiedAt": now.Format(time.RFC3339), "sourceCluster": "aws", "targetCluster": "aws", "trainingRuntimeRef": map[string]interface{}{"name": "runtime", "uid": "runtime-uid"}}}
	if seconds, _, ok := verifiedRecoveryDuration(req, input, now); !ok || seconds != 60 {
		t.Fatalf("valid evidence rejected: %v %v", seconds, ok)
	}
	for _, tc := range []struct {
		path  []string
		value interface{}
	}{
		{[]string{"status", "phase"}, "Running"},
		{[]string{"status", "observedGeneration"}, int64(1)},
		{[]string{"status", "verification", "requestUID"}, "old"},
		{[]string{"status", "verification", "checkpointID"}, "old"},
		{[]string{"status", "verification", "operation"}, "old"},
		{[]string{"status", "verification", "trainingRuntimeRef", "uid"}, "old"},
		{[]string{"status", "verification", "verifiedAt"}, now.Add(time.Minute).Format(time.RFC3339)},
		{[]string{"spec", "workloadRef", "uid"}, "old"},
	} {
		bad := req.DeepCopy()
		_ = unstructured.SetNestedField(bad.Object, tc.value, tc.path...)
		if _, _, ok := verifiedRecoveryDuration(bad, input, now); ok {
			t.Fatalf("invalid evidence accepted: %v", tc.path)
		}
	}
	r := PolicyReconciler{Client: fake.NewClientBuilder().WithRuntimeObjects(req).Build(), Clock: func() time.Time { return now }}
	risk := p.RiskSnapshot{Ready: true, LambdaPerHour: .01, SpotPricePerHour: .2, OnDemandPricePerHour: 1}
	if err := r.applyRecoveryEconomics(context.Background(), &input, p.RuntimeSnapshot{}, risk); err != nil {
		t.Fatal(err)
	}
	if input.Economics.LossCostPerEviction != -1 {
		t.Fatal("bootstrap interval treated as measured loss")
	}
	input.Checkpoint.MeasuredCosts = p.MeasuredCosts{CheckpointSeconds: 10, CopySeconds: 2, ObservedAt: now.Format(time.RFC3339)}
	if err := r.applyRecoveryEconomics(context.Background(), &input, p.RuntimeSnapshot{}, risk); err != nil {
		t.Fatal(err)
	}
	if input.Economics.LossCostPerEviction <= 0 || input.Economics.Source == "" {
		t.Fatal("verified recovery was not connected to economics")
	}
}
