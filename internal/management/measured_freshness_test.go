package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestLongMeasuredFreshnessPreservesEvidenceScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		want   bool
	}{
		{"retained evidence", func(m *unstructured.Unstructured) {
			m.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid", p.LabelRole: checkpointEvidenceRole})
		}, true},
		{"other policy", func(m *unstructured.Unstructured) {
			m.SetLabels(map[string]string{p.LabelPolicyUID: "other", p.LabelRole: "checkpoint"})
		}, false},
		{"other role", func(m *unstructured.Unstructured) {
			m.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid", p.LabelRole: "recovery"})
		}, false},
		{"stale generation", func(m *unstructured.Unstructured) { m.SetGeneration(3) }, false},
		{"invalid archive", func(m *unstructured.Unstructured) { mustSetFirstFileField(t, m, "bad", "sha256") }, false},
		{"invalid completion", func(m *unstructured.Unstructured) { mustSetClusterStatusField(t, m, "bad", "completionTime") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migration := measuredCostMigration()
			migration.SetName("verified-round")
			migration.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid", p.LabelRole: "checkpoint"})
			tc.mutate(migration)
			report := sourceStatus(t, migration)
			report["clusterName"] = "source"
			migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{report}}
			now := mustParseTime(t, "2026-09-26T00:02:00Z").Add(time.Hour)
			r := checkpointReconcilerFixture(t, func() time.Time { return now }, migration)
			input := p.PolicyInput{Namespace: "demo", PolicyUID: "policy-uid", SourceCluster: "source", Checkpoint: p.CheckpointPolicy{MaxIntervalSeconds: 3600}}
			_, _, _, costs, err := r.checkpointState(context.Background(), input)
			if err != nil || (costs != nil) != tc.want {
				t.Fatalf("costs=%+v err=%v want costs=%v", costs, err, tc.want)
			}
		})
	}
}

func TestCheckpointStateMeasuredFreshness(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  int64
		age  time.Duration
		want bool
	}{
		{"long interval", 3600, 3106 * time.Second, true},
		{"boundary", 3600, 4200 * time.Second, true},
		{"expired", 3600, 4201 * time.Second, false},
		{"default boundary", 0, 600 * time.Second, true},
		{"default expired", 0, 601 * time.Second, false},
		{"future", 3600, -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migration := measuredCostMigration()
			migration.SetName("verified-round")
			migration.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid", p.LabelRole: "checkpoint"})
			report := sourceStatus(t, migration)
			report["clusterName"] = "source"
			migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{report}}
			now := mustParseTime(t, "2026-09-26T00:02:00Z").Add(tc.age)
			r := checkpointReconcilerFixture(t, func() time.Time { return now }, migration)
			input := p.PolicyInput{Namespace: "demo", PolicyUID: "policy-uid", SourceCluster: "source", Checkpoint: p.CheckpointPolicy{MaxIntervalSeconds: tc.max}}
			_, inflight, _, costs, err := r.checkpointState(context.Background(), input)
			if err != nil || inflight || (costs != nil) != tc.want {
				t.Fatalf("costs=%+v inflight=%v err=%v want costs=%v", costs, inflight, err, tc.want)
			}
		})
	}
}
