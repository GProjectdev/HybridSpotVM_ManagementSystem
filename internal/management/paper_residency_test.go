package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestPaperResidencyRequiresVerifiedUIDAndExpires(t *testing.T) {
	now := time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
	in := p.PolicyInput{Namespace: "demo", PolicyUID: "policy", SourceCluster: "aws", RuntimeRefName: "runtime", WorkloadRef: p.WorkloadRef{UID: "workload"}, TargetWorkers: 2, MinOnDemand: 1, VMPolicyMethod: "paper-eq6-7", Economics: p.EconomicsPolicy{OnDemandDurationSeconds: 3600}}
	req := newRestoreRequest()
	req.SetName("restore")
	req.SetNamespace("demo")
	req.SetUID("rr")
	req.SetGeneration(1)
	req.SetCreationTimestamp(metav1.NewTime(now.Add(-2 * time.Minute)))
	req.SetLabels(map[string]string{p.LabelPolicyUID: "policy", p.LabelRole: groupRestoreRole})
	req.SetAnnotations(map[string]string{groupNewName: "od", groupNewUID: "node-uid"})
	req.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload"}, "checkpointRef": map[string]interface{}{"checkpointID": "cp"}, "groupRestore": map[string]interface{}{"operationUID": "op"}, "sourceCluster": "aws", "targetCluster": "aws", "trainingRuntimeRef": map[string]interface{}{"name": "runtime", "uid": "rt"}}
	req.Object["status"] = map[string]interface{}{"phase": "Verified", "observedGeneration": int64(1), "verification": map[string]interface{}{"requestUID": "rr", "operation": "op", "checkpointID": "cp", "sourceCluster": "aws", "targetCluster": "aws", "trainingRuntimeRef": map[string]interface{}{"name": "runtime", "uid": "rt"}, "verifiedAt": now.Add(-time.Minute).Format(time.RFC3339), "sourceFenced": true, "sourceFence": map[string]interface{}{"fenced": true, "operation": "op", "evidenceID": "fence", "observedAt": now.Add(-time.Minute).Format(time.RFC3339)}}}
	np := p.NewObject("NodeProvision")
	np.SetNamespace("demo")
	np.SetName("od")
	np.SetUID("node-uid")
	np.SetLabels(map[string]string{p.LabelPolicyUID: "policy"})
	np.Object["spec"] = map[string]interface{}{"marketType": "OnDemand"}
	np.Object["status"] = map[string]interface{}{"phase": "Ready"}
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		mutate  func(*unstructured.Unstructured, *unstructured.Unstructured)
		want    int64
	}{
		{"valid", 0, func(*unstructured.Unstructured, *unstructured.Unstructured) {}, 2},
		{"expired", time.Hour, func(*unstructured.Unstructured, *unstructured.Unstructured) {}, 1},
		{"recreated-node", 0, func(_ *unstructured.Unstructured, n *unstructured.Unstructured) { n.SetUID("recreated") }, 1},
		{"unverified", 0, func(q *unstructured.Unstructured, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(q.Object, "Running", "status", "phase")
		}, 1},
		{"other-workload", 0, func(q *unstructured.Unstructured, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(q.Object, "old", "spec", "workloadRef", "uid")
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, n := req.DeepCopy(), np.DeepCopy()
			tc.mutate(q, n)
			c := fake.NewClientBuilder().WithRuntimeObjects(q, n).Build()
			r := PolicyReconciler{Client: c, Clock: func() time.Time { return now.Add(tc.elapsed) }}
			d := p.Decision{OnDemandWorkers: 1, SpotWorkers: 1}
			if err := r.applyPaperResidency(context.Background(), in, p.RuntimeSnapshot{}, p.RiskSnapshot{}, &d); err != nil {
				t.Fatal(err)
			}
			if d.OnDemandWorkers != tc.want {
				t.Fatalf("%+v", d)
			}
		})
	}
}

func TestPaperResidencyHoldsEveryVerifiedBatchDestination(t *testing.T) {
	for _, mode := range []string{"all-three", "recreated-second", "expired", "unverified", "spot-destinations", "bad-receipt"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, policy, input, intent := batchFixture(t, mode == "spot-destinations")
			if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
				t.Fatal(err)
			}
			readyBatchTargets(t, r, intent, 3)
			r.GroupStage = groupStageRequest
			if _, err := r.ensureGroupBatchReplacement(ctx, policy, input, intent); err != nil {
				t.Fatal(err)
			}
			req := batchRequest(t, r, intent)
			markBatchVerified(t, r, req)
			now := time.Now().UTC().Truncate(time.Second)
			req.SetCreationTimestamp(metav1.NewTime(now.Add(-2 * time.Minute)))
			_ = unstructured.SetNestedField(req.Object, now.Add(-time.Minute).Format(time.RFC3339), "status", "verification", "verifiedAt")
			_ = unstructured.SetNestedField(req.Object, now.Add(-time.Minute).Format(time.RFC3339), "status", "verification", "sourceFence", "observedAt")
			want := int64(4)
			switch mode {
			case "recreated-second":
				n := p.NewObject("NodeProvision")
				if err := r.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: intent.Batch[1].Replacement}, n); err != nil {
					t.Fatal(err)
				}
				n.SetUID("recreated-destination")
				if err := r.Update(ctx, n); err != nil {
					t.Fatal(err)
				}
				want = 3
			case "expired":
				now = now.Add(time.Hour)
				want = 1
			case "unverified":
				_ = unstructured.SetNestedField(req.Object, "Restoring", "status", "phase")
				want = 1
			case "spot-destinations":
				want = 1
			case "bad-receipt":
				a := req.GetAnnotations()
				a[groupBatchPairsAnnotation] = "[]"
				req.SetAnnotations(a)
			}
			if err := r.Update(ctx, req); err != nil {
				t.Fatal(err)
			}
			input.MinOnDemand, input.VMPolicyMethod, input.SourceCluster = 1, "paper-eq6-7", "aws"
			input.Economics.OnDemandDurationSeconds = 3600
			r.Clock = func() time.Time { return now }
			d := p.Decision{OnDemandWorkers: 1, SpotWorkers: 3}
			err := r.applyPaperResidency(ctx, input, p.RuntimeSnapshot{}, p.RiskSnapshot{Ready: true, LambdaPerHour: .001}, &d)
			if mode == "bad-receipt" {
				if err == nil {
					t.Fatal("malformed batch receipt must block evaluation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.OnDemandWorkers != want || d.SpotWorkers != 4-want {
				t.Fatalf("%s: %+v", mode, d)
			}
			if mode == "all-three" && (d.CheckpointIntervalSeconds != 3600 || d.IntervalCostEvaluated || d.IntervalSource != "hourly-risk-refresh-fallback") {
				t.Fatalf("all-OD residency must retain an hourly recovery point, not a Spot cost optimum: %+v", d)
			}
		})
	}
}
