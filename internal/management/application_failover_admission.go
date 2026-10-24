package management

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const applicationFailoverSourceAnnotation = "training.dcnlab.com/application-failover-source"

func applicationFailoverClusters(binding *unstructured.Unstructured) ([]interface{}, error) {
	value, _, err := unstructured.NestedFieldNoCopy(binding.Object, "spec", "clusters")
	if err != nil || value == nil {
		return nil, err
	}
	clusters, _, err := unstructured.NestedSlice(binding.Object, "spec", "clusters")
	return clusters, err
}

// applicationFailoverSourcePlacement recovers the source only while placement is empty.
func applicationFailoverSourcePlacement(binding *unstructured.Unstructured) ([]interface{}, error) {
	clusters, err := applicationFailoverClusters(binding)
	if err != nil || len(clusters) != 0 {
		return clusters, err
	}
	if raw := binding.GetAnnotations()[applicationFailoverSourceAnnotation]; raw != "" {
		if err := utiljson.Unmarshal([]byte(raw), &clusters); err != nil {
			return nil, fmt.Errorf("invalid application failover source: %w", err)
		}
		if len(clusters) != 1 {
			return nil, fmt.Errorf("single application failover source required")
		}
		source, ok := clusters[0].(map[string]interface{})
		if !ok || stringField(source, "name") == "" {
			return nil, fmt.Errorf("invalid application failover source placement")
		}
	}
	return clusters, nil
}

func sameApplicationFailoverIdentity(old, next *unstructured.Unstructured) error {
	if old.GetUID() == "" || old.GetUID() != next.GetUID() || old.GetNamespace() != next.GetNamespace() || old.GetName() != next.GetName() {
		return fmt.Errorf("application failover binding identity changed")
	}
	for _, key := range []string{"apiVersion", "kind", "name", "namespace", "uid"} {
		if stringField(old.Object, "spec", "resource", key) != stringField(next.Object, "spec", "resource", key) {
			return fmt.Errorf("application failover workload identity changed")
		}
	}
	return nil
}

func applicationFailoverTask(binding *unstructured.Unstructured, source string) ([]interface{}, error) {
	tasks, _, err := unstructured.NestedSlice(binding.Object, "spec", "gracefulEvictionTasks")
	if err != nil || len(tasks) != 1 {
		return nil, fmt.Errorf("exactly one source application failover task required")
	}
	task, ok := tasks[0].(map[string]interface{})
	if !ok || stringField(task, "fromCluster") != source || stringField(task, "reason") != "ApplicationFailure" || stringField(task, "purgeMode") != "Never" || !boolField(task, "suppressDeletion") {
		return nil, fmt.Errorf("source task must be ApplicationFailure Never with suppressDeletion")
	}
	if _, present := task["gracePeriodSeconds"]; present {
		return nil, fmt.Errorf("source task cannot have a grace period")
	}
	return tasks, nil
}

func preserveApplicationFailoverHold(old, next *unstructured.Unstructured) error {
	before, _, err := unstructured.NestedMap(old.Object, "spec", "suspension")
	if err != nil {
		return err
	}
	after, _, err := unstructured.NestedMap(next.Object, "spec", "suspension")
	if err != nil || !reflect.DeepEqual(before, after) {
		return fmt.Errorf("application failover dispatch suspension cannot change before release")
	}
	for _, key := range []string{groupVolumeHold, "migration.dcnlab.com/pv-migration", "migration.dcnlab.com/pv-migration-uid"} {
		if old.GetAnnotations()[key] != next.GetAnnotations()[key] {
			return fmt.Errorf("application failover volume hold or receipt cannot change before release")
		}
	}
	return nil
}

func (h *PlacementAdmission) applicationFailoverRequest(ctx context.Context, policy, binding *unstructured.Unstructured, source string) (*unstructured.Unstructured, error) {
	a := binding.GetAnnotations()
	if binding.GetUID() == "" || a[applicationFailoverTrigger] != string(binding.GetUID()) || stringField(policy.Object, "spec", "capacity", "mode") != "Existing" || policy.GetUID() == "" || !policy.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("application failover requires Existing capacity and explicit binding UID trigger")
	}
	request := newRestoreRequest()
	if a[placementRequestAnnotation] == "" || a[placementRequestUIDAnnotation] == "" {
		return nil, fmt.Errorf("application failover bound restore request missing")
	}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: binding.GetNamespace(), Name: a[placementRequestAnnotation]}, request); err != nil {
		return nil, err
	}
	target := stringField(policy.Object, "spec", "capacity", "aws", "karmadaCluster")
	if target == "" {
		target = p.DefaultAWSCluster
	}
	if string(request.GetUID()) != a[placementRequestUIDAnnotation] || request.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) || stringField(request.Object, "spec", "groupRestore", "operationUID") != string(binding.GetUID()) {
		return nil, fmt.Errorf("application failover restore ownership mismatch")
	}
	if hold := a[groupVolumeHold]; hold != "" && (hold != string(request.GetUID()) || !boolField(binding.Object, "spec", "suspension", "dispatching")) {
		return nil, fmt.Errorf("application failover group volume dispatch hold invalid")
	}
	if source == "" || target == source || stringField(policy.Object, "spec", "sourceCluster") != source || stringField(request.Object, "spec", "sourceCluster") != source || stringField(request.Object, "spec", "targetCluster") != target {
		return nil, fmt.Errorf("application failover restore route mismatch")
	}
	for _, key := range []string{"name", "uid"} {
		value := stringField(binding.Object, "spec", "resource", key)
		if value == "" || stringField(request.Object, "spec", "workloadRef", key) != value || stringField(policy.Object, "spec", "workloadRef", key) != value {
			return nil, fmt.Errorf("application failover workload mismatch")
		}
	}
	if err := validateGroupRelease(ctx, h.Reader, request); err != nil {
		return nil, err
	}
	if err := validateGroupVolumes(ctx, h.Reader, binding, request); err != nil {
		return nil, err
	}
	return request, nil
}

func (h *PlacementAdmission) holdApplicationFailover(ctx context.Context, policy, old, next *unstructured.Unstructured, username string) (bool, error) {
	before, err := applicationFailoverClusters(old)
	if err != nil {
		return false, err
	}
	after, err := applicationFailoverClusters(next)
	if err != nil {
		return false, err
	}
	snapshot := old.GetAnnotations()[applicationFailoverSourceAnnotation]
	if snapshot == "" {
		if next.GetAnnotations()[applicationFailoverSourceAnnotation] != "" {
			return false, fmt.Errorf("application failover source snapshot cannot be supplied externally")
		}
		if old.GetAnnotations()[applicationFailoverTrigger] == "" || len(before) != 1 || len(after) != 0 {
			return false, nil
		}
		if err := sameApplicationFailoverIdentity(old, next); err != nil {
			return false, err
		}
		source, ok := before[0].(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("invalid application failover source")
		}
		oldTasks, _, err := unstructured.NestedSlice(old.Object, "spec", "gracefulEvictionTasks")
		if err != nil || len(oldTasks) != 0 || old.GetAnnotations()[pendingPlacementAnnotation] != "" || next.GetAnnotations()[pendingPlacementAnnotation] != "" {
			return false, fmt.Errorf("application failover already has eviction or placement intent")
		}
		if _, err := applicationFailoverTask(next, stringField(source, "name")); err != nil {
			return false, err
		}
		if _, err := h.applicationFailoverRequest(ctx, policy, old, stringField(source, "name")); err != nil {
			return false, err
		}
		if err := preserveApplicationFailoverHold(old, next); err != nil {
			return false, err
		}
		for _, key := range []string{applicationFailoverTrigger, placementRequestAnnotation, placementRequestUIDAnnotation} {
			if old.GetAnnotations()[key] != next.GetAnnotations()[key] {
				return false, fmt.Errorf("application failover request identity changed")
			}
		}
		raw, err := json.Marshal(before)
		if err != nil {
			return false, err
		}
		a := next.GetAnnotations()
		a[applicationFailoverSourceAnnotation] = string(raw)
		next.SetAnnotations(a)
		return true, nil
	}
	if err := sameApplicationFailoverIdentity(old, next); err != nil {
		return false, err
	}
	if len(before) != 0 {
		return false, fmt.Errorf("application failover bridge must retain empty placement")
	}
	sources, err := applicationFailoverSourcePlacement(old)
	if err != nil {
		return false, err
	}
	source := stringField(sources[0].(map[string]interface{}), "name")
	tasks, err := applicationFailoverTask(old, source)
	if err != nil {
		return false, err
	}
	nextTasks, _, err := unstructured.NestedSlice(next.Object, "spec", "gracefulEvictionTasks")
	if err != nil || !reflect.DeepEqual(tasks, nextTasks) {
		return false, fmt.Errorf("approved application failover eviction task is immutable")
	}
	a := next.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	if a[applicationFailoverSourceAnnotation] != "" && a[applicationFailoverSourceAnnotation] != snapshot {
		return false, fmt.Errorf("application failover source snapshot is immutable")
	}
	pending := old.GetAnnotations()[pendingPlacementAnnotation]
	if pending != "" && a[pendingPlacementAnnotation] == "" && len(after) != 0 {
		if h.ReleaseUsername == "" || username != h.ReleaseUsername {
			return false, fmt.Errorf("only the placement controller may release application failover")
		}
		if _, err := h.applicationFailoverRequest(ctx, policy, old, source); err != nil {
			return false, err
		}
		if err := h.validateRelease(ctx, policy, old, next); err != nil {
			return false, err
		}
		// Release must consume the snapshot so ordinary later updates use the generic path.
		delete(a, applicationFailoverSourceAnnotation)
		next.SetAnnotations(a)
		return true, nil
	}
	if err := preserveApplicationFailoverHold(old, next); err != nil {
		return false, err
	}
	a[applicationFailoverSourceAnnotation] = snapshot
	for _, key := range []string{applicationFailoverTrigger, placementRequestAnnotation, placementRequestUIDAnnotation} {
		if a[key] != "" && a[key] != old.GetAnnotations()[key] {
			return false, fmt.Errorf("application failover request identity is immutable")
		}
		a[key] = old.GetAnnotations()[key]
	}
	if len(after) == 0 {
		if a[pendingPlacementAnnotation] != "" && a[pendingPlacementAnnotation] != pending {
			return false, fmt.Errorf("pending placement intent is immutable")
		}
		if pending != "" {
			a[pendingPlacementAnnotation] = pending
		}
		next.SetAnnotations(a)
		return true, nil
	}
	request, err := h.applicationFailoverRequest(ctx, policy, old, source)
	if err != nil {
		return false, err
	}
	if len(after) != 1 {
		return false, fmt.Errorf("single application failover target required")
	}
	target, ok := after[0].(map[string]interface{})
	if !ok || stringField(target, "name") != stringField(request.Object, "spec", "targetCluster") {
		return false, fmt.Errorf("application failover scheduler target mismatch")
	}
	if replicas, present := target["replicas"]; present && replicas != intField(request.Object, "spec", "groupRestore", "worldSize") {
		return false, fmt.Errorf("application failover requires full world placement")
	}
	raw, err := json.Marshal(after)
	if err != nil {
		return false, err
	}
	if pending != "" && pending != string(raw) {
		return false, fmt.Errorf("another application failover placement is pending")
	}
	a[pendingPlacementAnnotation] = string(raw)
	next.SetAnnotations(a)
	return true, unstructured.SetNestedSlice(next.Object, []interface{}{}, "spec", "clusters")
}
