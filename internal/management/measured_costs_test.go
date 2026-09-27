package management

import (
	"strings"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestMeasuredCostsFromMigrationUsesProducerStatusAndVerifiedArchives(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:03:30Z")
	migration := measuredCostMigration()

	costs, ok, reason := measuredCostsFromMigration(migration, "source", now)
	if !ok {
		t.Fatalf("measuredCostsFromMigration() rejected evidence: %s", reason)
	}
	if costs.CheckpointSeconds != 90 || costs.CopySeconds != 30 || costs.ObservedAt != "2026-09-26T00:02:00Z" {
		t.Fatalf("costs = %#v, want checkpoint=90 copy=30 observedAt latest export", costs)
	}
}

func TestMeasuredCostsFromMigrationFallsBackToPerFileCheckpointTime(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:03:30Z")
	migration := measuredCostMigration()
	mustSetClusterStatusField(t, migration, "", "completionTime")

	costs, ok, reason := measuredCostsFromMigration(migration, "source", now)
	if !ok {
		t.Fatalf("per-file checkpointTime rejected: %s", reason)
	}
	if costs.CheckpointSeconds != 95 || costs.CopySeconds != 25 {
		t.Fatalf("costs = %#v, want latest checkpointTime fallback", costs)
	}
}

func TestMeasuredCostsFromMigrationRejectsMissingStaleAndMalformedArchiveEvidence(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:03:30Z")
	base := measuredCostMigration()

	tests := []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		want   string
		maxAge time.Duration
	}{
		{"missing startTime", func(obj *unstructured.Unstructured) { mustSetClusterStatusField(t, obj, "", "startTime") }, "missing_start_time", 0},
		{"stale generation", func(obj *unstructured.Unstructured) {
			mustSetClusterStatusField(t, obj, int64(1), "observedGeneration")
		}, "stale_migration_generation", 0},
		{"missing checkpoint files", func(obj *unstructured.Unstructured) {
			mustSetPods(t, obj, []interface{}{map[string]interface{}{"podName": "rank-0"}})
		}, "missing_checkpoint_files", 0},
		{"bad hash", func(obj *unstructured.Unstructured) { mustSetFirstFileField(t, obj, "abc", "sha256") }, "malformed_checkpoint_file", 0},
		{"bad durable ref", func(obj *unstructured.Unstructured) { mustSetFirstFileField(t, obj, "s3://bucket/key", "durableRef") }, "malformed_checkpoint_file", 0},
		{"wrong durable namespace", func(obj *unstructured.Unstructured) {
			mustSetFirstFileField(t, obj, "file-store:other/sha256/"+strings.Repeat("a", 64), "durableRef")
		}, "malformed_checkpoint_file", 0},
		{"wrong durable hash", func(obj *unstructured.Unstructured) {
			mustSetFirstFileField(t, obj, "file-store:demo/sha256/"+strings.Repeat("c", 64), "durableRef")
		}, "malformed_checkpoint_file", 0},
		{"malformed pod entry", func(obj *unstructured.Unstructured) { mustSetPods(t, obj, []interface{}{"rank-0"}) }, "missing_pod_archive_evidence", 0},
		{"malformed file entry", func(obj *unstructured.Unstructured) {
			mustSetPods(t, obj, []interface{}{map[string]interface{}{"podName": "rank-0", "checkpointFiles": []interface{}{"file"}}})
		}, "missing_checkpoint_files", 0},
		{"missing checkpointTime", func(obj *unstructured.Unstructured) { mustSetFirstFileField(t, obj, "", "checkpointTime") }, "missing_checkpoint_time", 0},
		{"checkpoint before startTime", func(obj *unstructured.Unstructured) {
			mustSetFirstFileField(t, obj, "2026-09-25T23:59:59Z", "checkpointTime")
		}, "invalid_archive_time_order", 0},
		{"export before checkpointTime", func(obj *unstructured.Unstructured) {
			mustSetFirstFileField(t, obj, "2026-09-26T00:01:00Z", "exportedAt")
		}, "invalid_archive_time_order", 0},
		{"zero checkpoint seconds", func(obj *unstructured.Unstructured) {
			mustSetClusterStatusField(t, obj, "2026-09-26T00:00:00Z", "completionTime")
		}, "invalid_measured_costs", 0},
		{"stale observation", func(obj *unstructured.Unstructured) {
			mustSetAllFileField(t, obj, "2026-09-26T00:02:00Z", "exportedAt")
		}, "stale_measured_costs", time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			migration := base.DeepCopy()
			tt.mutate(migration)
			maxAge := tt.maxAge
			if maxAge == 0 {
				maxAge = 10 * time.Minute
			}
			_, ok, reason := measuredCostsFromMigrationWithMaxAge(migration, "source", now, maxAge)
			if ok || reason != tt.want {
				t.Fatalf("ok=%v reason=%q, want rejection %q", ok, reason, tt.want)
			}
		})
	}
}

func TestMeasuredCostsFromMigrationAllowsLegitimateZeroCopyDuration(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:03:30Z")
	migration := measuredCostMigration()
	mustSetAllFileField(t, migration, "2026-09-26T00:01:30Z", "checkpointTime")
	mustSetAllFileField(t, migration, "2026-09-26T00:01:30Z", "exportedAt")

	costs, ok, reason := measuredCostsFromMigration(migration, "source", now)
	if !ok {
		t.Fatalf("zero copy duration rejected: %s", reason)
	}
	if costs.CopySeconds != 0 {
		t.Fatalf("copySeconds = %v, want legitimate zero", costs.CopySeconds)
	}
}

func TestReadPolicySpecPrefersStatusMeasuredCosts(t *testing.T) {
	policy := trainingpolicy.NewObject("TrainingPolicy")
	policy.Object["spec"] = map[string]interface{}{"checkpoint": map[string]interface{}{"measuredCosts": map[string]interface{}{"checkpointSeconds": float64(9), "copySeconds": float64(3), "observedAt": "2026-09-26T00:00:00Z"}}}
	policy.Object["status"] = map[string]interface{}{trainingpolicy.StatusCheckpointPath: map[string]interface{}{"measuredCosts": map[string]interface{}{"checkpointSeconds": float64(90), "copySeconds": float64(30), "observedAt": "2026-09-26T00:02:00Z"}}}

	input := trainingpolicy.ReadPolicySpec(policy)
	if input.Checkpoint.MeasuredCosts.CheckpointSeconds != 90 || input.Checkpoint.MeasuredCosts.CopySeconds != 30 {
		t.Fatalf("measured costs = %#v, want status override", input.Checkpoint.MeasuredCosts)
	}
}

func measuredCostMigration() *unstructured.Unstructured {
	migration := trainingpolicy.NewObject("FluidCRMigration")
	migration.SetNamespace("demo")
	migration.SetGeneration(2)
	migration.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{
		"clusterName": "source",
		"status": map[string]interface{}{
			"phase":              "Completed",
			"observedGeneration": int64(2),
			"startTime":          "2026-09-26T00:00:00Z",
			"completionTime":     "2026-09-26T00:01:30Z",
			"pods": []interface{}{
				map[string]interface{}{"podName": "rank-0", "checkpointFiles": []interface{}{archiveFile("a", "2026-09-26T00:01:20Z", "2026-09-26T00:01:45Z")}},
				map[string]interface{}{"podName": "rank-1", "checkpointFiles": []interface{}{archiveFile("b", "2026-09-26T00:01:35Z", "2026-09-26T00:02:00Z")}},
			},
		},
	}}}
	return migration
}

func archiveFile(seed, checkpointTime, exportedAt string) map[string]interface{} {
	sha := strings.Repeat(seed, 64)
	return map[string]interface{}{"sha256": sha, "durableRef": "file-store:demo/sha256/" + sha, "checkpointTime": checkpointTime, "exportedAt": exportedAt}
}

func sourceStatus(t *testing.T, obj *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	status, ok := obj.Object["status"].(map[string]interface{})
	if !ok {
		t.Fatalf("status missing or wrong type")
	}
	clusters, ok := status["clusters"].([]interface{})
	if !ok || len(clusters) == 0 {
		t.Fatalf("status.clusters missing or empty")
	}
	cluster, ok := clusters[0].(map[string]interface{})
	if !ok {
		t.Fatalf("status.clusters[0] wrong type")
	}
	clusterStatus, ok := cluster["status"].(map[string]interface{})
	if !ok {
		t.Fatalf("status.clusters[0].status missing or wrong type")
	}
	return clusterStatus
}

func mustSetClusterStatusField(t *testing.T, obj *unstructured.Unstructured, value interface{}, field string) {
	t.Helper()
	status := sourceStatus(t, obj)
	status[field] = value
}

func mustSetPods(t *testing.T, obj *unstructured.Unstructured, pods []interface{}) {
	t.Helper()
	status := sourceStatus(t, obj)
	status["pods"] = pods
}

func mustSetFirstFileField(t *testing.T, obj *unstructured.Unstructured, value interface{}, field string) {
	t.Helper()
	status := sourceStatus(t, obj)
	pods := status["pods"].([]interface{})
	files := pods[0].(map[string]interface{})["checkpointFiles"].([]interface{})
	files[0].(map[string]interface{})[field] = value
}

func mustSetAllFileField(t *testing.T, obj *unstructured.Unstructured, value interface{}, field string) {
	t.Helper()
	status := sourceStatus(t, obj)
	pods := status["pods"].([]interface{})
	for _, pod := range pods {
		files := pod.(map[string]interface{})["checkpointFiles"].([]interface{})
		for _, file := range files {
			file.(map[string]interface{})[field] = value
		}
	}
}
