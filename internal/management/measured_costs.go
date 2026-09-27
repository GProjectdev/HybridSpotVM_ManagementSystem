package management

import (
	"fmt"
	"math"
	"regexp"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const defaultMeasuredCostMaxAge = 10 * time.Minute

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func measuredCostsFromMigration(migration *unstructured.Unstructured, sourceCluster string, now time.Time) (trainingpolicy.MeasuredCosts, bool, string) {
	return measuredCostsFromMigrationWithMaxAge(migration, sourceCluster, now, defaultMeasuredCostMaxAge)
}

func measuredCostsFromMigrationWithMaxAge(migration *unstructured.Unstructured, sourceCluster string, now time.Time, maxAge time.Duration) (trainingpolicy.MeasuredCosts, bool, string) {
	if migration == nil {
		return trainingpolicy.MeasuredCosts{}, false, "missing_migration"
	}
	cluster, ok := sourceClusterStatus(migration, sourceCluster)
	if !ok {
		return trainingpolicy.MeasuredCosts{}, false, "missing_source_cluster_status"
	}
	if stringFromStatus(cluster, "phase") != "Completed" {
		return trainingpolicy.MeasuredCosts{}, false, "source_cluster_not_completed"
	}
	observedGeneration := intFromStatus(cluster, "observedGeneration")
	if generation := migration.GetGeneration(); generation > 0 && observedGeneration != generation {
		return trainingpolicy.MeasuredCosts{}, false, "stale_migration_generation"
	}
	startTime, err := parseRequiredRFC3339(stringFromStatus(cluster, "startTime"))
	if err != nil {
		return trainingpolicy.MeasuredCosts{}, false, "missing_start_time"
	}
	archiveTiming, ok, reason := verifiedArchiveTiming(cluster, migration.GetNamespace(), startTime)
	if !ok {
		return trainingpolicy.MeasuredCosts{}, false, reason
	}
	checkpointDone := archiveTiming.latestCheckpointTime
	if completion := stringFromStatus(cluster, "completionTime"); completion != "" {
		checkpointDone, err = parseRequiredRFC3339(completion)
		if err != nil {
			return trainingpolicy.MeasuredCosts{}, false, "invalid_completion_time"
		}
	}
	observedAt := archiveTiming.latestExportedAt
	if observedAt.After(now) || now.Sub(observedAt) > maxAge {
		return trainingpolicy.MeasuredCosts{}, false, "stale_measured_costs"
	}
	checkpointSeconds := checkpointDone.Sub(startTime).Seconds()
	copySeconds := observedAt.Sub(checkpointDone).Seconds()
	if !positiveFinite(checkpointSeconds) || copySeconds < 0 || !finiteFloat(copySeconds) {
		return trainingpolicy.MeasuredCosts{}, false, "invalid_measured_costs"
	}
	return trainingpolicy.MeasuredCosts{
		CheckpointSeconds: checkpointSeconds,
		CopySeconds:       copySeconds,
		ObservedAt:        observedAt.UTC().Format(time.RFC3339),
	}, true, "measured_from_fluidcr_status"
}

type archiveTiming struct {
	latestCheckpointTime time.Time
	latestExportedAt     time.Time
}

func verifiedArchiveTiming(cluster map[string]interface{}, namespace string, startTime time.Time) (archiveTiming, bool, string) {
	pods, podsOK := mapSliceFromStatus(cluster, "pods")
	if !podsOK || len(pods) == 0 {
		return archiveTiming{}, false, "missing_pod_archive_evidence"
	}
	latestCheckpoint := time.Time{}
	latestExport := time.Time{}
	validFiles := 0
	for _, pod := range pods {
		files, filesOK := mapSliceFromStatus(pod, "checkpointFiles")
		if !filesOK || len(files) == 0 {
			return archiveTiming{}, false, "missing_checkpoint_files"
		}
		for _, file := range files {
			if !validArchiveFile(file, namespace) {
				return archiveTiming{}, false, "malformed_checkpoint_file"
			}
			checkpointTime, err := parseRequiredRFC3339(stringField(file, "checkpointTime"))
			if err != nil {
				return archiveTiming{}, false, "missing_checkpoint_time"
			}
			exportedAt, err := parseRequiredRFC3339(stringField(file, "exportedAt"))
			if err != nil {
				return archiveTiming{}, false, "missing_exported_at"
			}
			if checkpointTime.Before(startTime) || exportedAt.Before(checkpointTime) {
				return archiveTiming{}, false, "invalid_archive_time_order"
			}
			if latestCheckpoint.IsZero() || checkpointTime.After(latestCheckpoint) {
				latestCheckpoint = checkpointTime
			}
			if latestExport.IsZero() || exportedAt.After(latestExport) {
				latestExport = exportedAt
			}
			validFiles++
		}
	}
	if validFiles == 0 {
		return archiveTiming{}, false, "missing_checkpoint_files"
	}
	return archiveTiming{latestCheckpointTime: latestCheckpoint, latestExportedAt: latestExport}, true, ""
}

func validArchiveFile(file map[string]interface{}, namespace string) bool {
	sha := stringField(file, "sha256")
	ref := stringField(file, "durableRef")
	if namespace == "" || !sha256Pattern.MatchString(sha) {
		return false
	}
	return ref == "file-store:"+namespace+"/sha256/"+sha
}

func sourceClusterStatus(migration *unstructured.Unstructured, sourceCluster string) (map[string]interface{}, bool) {
	var selected map[string]interface{}
	found := 0
	for _, cluster := range nestedClusterStatuses(migration.Object) {
		name, _, _ := unstructured.NestedString(cluster, "clusterName")
		if name != sourceCluster {
			continue
		}
		found++
		selected = cluster
	}
	return selected, found == 1
}

func parseRequiredRFC3339(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func positiveFinite(value float64) bool {
	return value > 0 && finiteFloat(value)
}

func finiteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func stringFromStatus(cluster map[string]interface{}, field string) string {
	if value, _, _ := unstructured.NestedString(cluster, field); value != "" {
		return value
	}
	value, _, _ := unstructured.NestedString(cluster, "status", field)
	return value
}

func intFromStatus(cluster map[string]interface{}, field string) int64 {
	if value, _, _ := unstructured.NestedInt64(cluster, field); value != 0 {
		return value
	}
	value, _, _ := unstructured.NestedInt64(cluster, "status", field)
	return value
}

func mapSliceFromStatus(obj map[string]interface{}, field string) ([]map[string]interface{}, bool) {
	if raw, ok, _ := unstructured.NestedSlice(obj, field); ok {
		return mapSlice(raw)
	}
	raw, ok, _ := unstructured.NestedSlice(obj, "status", field)
	if !ok {
		return nil, false
	}
	return mapSlice(raw)
}

func mapSlice(raw []interface{}) ([]map[string]interface{}, bool) {
	out := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		typed, ok := item.(map[string]interface{})
		if !ok {
			return nil, false
		}
		out = append(out, typed)
	}
	return out, true
}
