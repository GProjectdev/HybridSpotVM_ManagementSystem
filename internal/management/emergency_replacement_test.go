package management

import (
	"context"
	"strings"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEmergencyReplacementNeedsDurableGroupRoundAndNoPeriodicCheckpoint(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy, runtime, node := emergencyReplacementFixtures(now)
	r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtime, node, workloadFixture("workload-uid"))
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	list := newSpotReplacementList()
	if err := r.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("operations: %d", len(list.Items))
	}
 updated:=p.NewObject("TrainingPolicy")
 if err:=r.Get(context.Background(),client.ObjectKeyFromObject(policy),updated);err!=nil{t.Fatal(err)}
 if !strings.Contains(stringField(updated.Object,"status","checkpoint","message"),"no complete durable full-group checkpoint"){t.Fatalf("missing group checkpoint not reported: %#v",updated.Object["status"])}
	assertMigrationCount(t, r.Client, 0)
}

func TestEmergencyReplacementRejectsUnsafeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured)
		want   string
	}{
		{"rank-zero", func(_, runtime, node *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(node.Object, "node-0", "status", "nodeName")
		}, "no complete durable full-group checkpoint"},
		{"stale", func(_, runtime, _ *unstructured.Unstructured) {
			clusters, _, _ := unstructured.NestedSlice(runtime.Object, "status", "clusters")
			clusters[0].(map[string]interface{})["status"].(map[string]interface{})["observedAt"] = "2020-01-01T00:00:00Z"
			_ = unstructured.SetNestedSlice(runtime.Object, clusters, "status", "clusters")
		}, "no complete durable full-group checkpoint"},
		{"on-demand-signal", func(_, _, node *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(node.Object, "OnDemand", "spec", "marketType")
		}, "Spot NodeProvision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := mustParseTime(t, "2026-09-26T00:00:00Z")
			policy, runtime, node := emergencyReplacementFixtures(now)
			tc.mutate(policy, runtime, node)
			r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtime, node, workloadFixture("workload-uid"))
			_, err := r.ensureEmergencyReplacement(context.Background(), policy, p.ReadPolicySpec(policy))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %s", err, tc.want)
			}
			ops := newSpotReplacementList()
			if err := r.List(context.Background(), ops); err != nil {
				t.Fatal(err)
			}
			if len(ops.Items) != 0 {
				t.Fatal("unsafe operation created")
			}
		})
	}
}

func TestEmergencyReplacementSkipsDisabledAndStaleInstance(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		now := mustParseTime(t, "2026-09-26T00:00:00Z")
		policy, runtime, node := emergencyReplacementFixtures(now)
		if disabled {
			_ = unstructured.SetNestedField(policy.Object, false, "spec", "replacement", "enabled")
		} else {
			_ = unstructured.SetNestedField(node.Object, "i-stale", "status", "spot", "instanceID")
		}
		r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtime, node)
		name, err := r.ensureEmergencyReplacement(context.Background(), policy, p.ReadPolicySpec(policy))
		if name != "" || err != nil {
			t.Fatalf("name=%q err=%v", name, err)
		}
		ops := newSpotReplacementList()
		if err := r.List(context.Background(), ops); err != nil {
			t.Fatal(err)
		}
		if len(ops.Items) != 0 {
			t.Fatal("unexpected replacement")
		}
	}
}

func TestEmergencyNodeWatchMapsOnlyOwningPolicy(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:00:00Z")
	policy, _, node := emergencyReplacementFixtures(now)
	r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy)
	if got := mapPolicies(r.Client, "node")(context.Background(), node); len(got) != 1 || got[0].Name != policy.GetName() {
		t.Fatalf("requests: %#v", got)
	}
	node.SetLabels(map[string]string{p.LabelPolicyUID: "foreign"})
	if got := mapPolicies(r.Client, "node")(context.Background(), node); len(got) != 0 {
		t.Fatalf("foreign requests: %#v", got)
	}
}

func emergencyReplacementFixtures(now time.Time) (*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	policy := checkpointPolicyFixture(now)
	_ = unstructured.SetNestedField(policy.Object, true, "spec", "replacement", "enabled")
	_ = unstructured.SetNestedField(policy.Object, "aws", "spec", "sourceCluster")
	_ = unstructured.SetNestedField(policy.Object, "aws", "spec", "capacity", "aws", "karmadaCluster")
	_ = unstructured.SetNestedField(policy.Object, int64(2), "spec", "targetWorkers")
	runtime := runtimeFixtureWithPods(now, []interface{}{
		map[string]interface{}{"name": "trainer-0", "uid": "pod-0", "nodeName": "node-0", "rank": int64(0), "checkpointID": "round", "observedAt": now.Format(time.RFC3339)},
		map[string]interface{}{"name": "trainer-1", "uid": "pod-1", "nodeName": "node-1", "rank": int64(1), "checkpointID": "round", "observedAt": now.Format(time.RFC3339)},
	})
	_ = unstructured.SetNestedField(runtime.Object, "aws", "spec", "sourceCluster")
	clusters, _, _ := unstructured.NestedSlice(runtime.Object, "status", "clusters")
	clusters[0].(map[string]interface{})["clusterName"] = "aws"
	_ = unstructured.SetNestedSlice(runtime.Object, clusters, "status", "clusters")
	node := emergencyNodeProvision("node-1", "node-uid", "policy-uid", "aws", "i-123", "notice")
	_ = unstructured.SetNestedField(node.Object, spotInterruptionNotice, "status", "spot", "signalType")
	_ = unstructured.SetNestedField(node.Object, "Spot", "spec", "marketType")
	_ = unstructured.SetNestedField(node.Object, "node-1", "status", "nodeName")
	return policy, runtime, node
}
