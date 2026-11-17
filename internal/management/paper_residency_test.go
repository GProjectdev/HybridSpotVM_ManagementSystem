package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
