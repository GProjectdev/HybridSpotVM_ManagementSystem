package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const recoverSelectedPlacement = "training.dcnlab.com/recover-selected-placement"

func groupPlacementRequestName(policy, operation, target string) string {
	hash := sha256.Sum256([]byte(operation + "/" + target))
	return policy + "-move-" + hex.EncodeToString(hash[:6])
}

// Reconstruct the held scheduling intent, never release dispatch. This explicit
// repair is for a scheduler transition made while placement admission was absent.
func recoverSelectedBinding(ctx context.Context, reader client.Reader, policy, binding *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	a := binding.GetAnnotations()
	if binding.GetUID() == "" || a[recoverSelectedPlacement] != string(binding.GetUID()) ||
		a[applicationFailoverTrigger] != string(binding.GetUID()) ||
		a[pendingPlacementAnnotation] != "" || a[applicationFailoverSourceAnnotation] != "" ||
		!binding.GetDeletionTimestamp().IsZero() || !policy.GetDeletionTimestamp().IsZero() ||
		stringField(policy.Object, "spec", "capacity", "mode") != p.CapacityModeExisting {
		return nil, fmt.Errorf("selected placement recovery requires explicit current binding UID and Existing capacity")
	}
	source := stringField(policy.Object, "spec", "sourceCluster")
	target := stringField(policy.Object, "spec", "capacity", "aws", "karmadaCluster")
	if target == "" {
		target = p.DefaultAWSCluster
	}
	clusters, err := applicationFailoverClusters(binding)
	if err != nil || len(clusters) != 1 {
		return nil, fmt.Errorf("single selected target required")
	}
	selected, ok := clusters[0].(map[string]interface{})
	if !ok || source == "" || source == target || stringField(selected, "name") != target {
		return nil, fmt.Errorf("selected recovery route mismatch")
	}
	if _, err := applicationFailoverTask(binding, source); err != nil {
		return nil, err
	}
	if a[placementRequestAnnotation] == "" || a[placementRequestUIDAnnotation] == "" ||
		a[groupVolumeHold] != a[placementRequestUIDAnnotation] ||
		!boolField(binding.Object, "spec", "suspension", "dispatching") {
		return nil, fmt.Errorf("exact restore-owned dispatch hold required")
	}
	request := newRestoreRequest()
	if a[placementRequestAnnotation] != groupPlacementRequestName(policy.GetName(), string(binding.GetUID()), target) {
		return nil, fmt.Errorf("bound request name differs from placement operation")
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: binding.GetNamespace(), Name: a[placementRequestAnnotation]}, request); err != nil {
		return nil, err
	}
	if phase := stringField(request.Object, "status", "phase"); phase != "Preparing" && phase != "Prepared" && phase != "AwaitingSourceFence" {
		return nil, fmt.Errorf("selected recovery requires a not-yet-running restore")
	}
	if string(request.GetUID()) != a[placementRequestUIDAnnotation] || policy.GetUID() == "" ||
		request.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) ||
		stringField(request.Object, "spec", "groupRestore", "operationUID") != string(binding.GetUID()) ||
		stringField(request.Object, "spec", "sourceCluster") != source ||
		stringField(request.Object, "spec", "targetCluster") != target {
		return nil, fmt.Errorf("selected recovery request identity mismatch")
	}
	for _, key := range []string{"name", "uid"} {
		value := stringField(binding.Object, "spec", "resource", key)
		if value == "" || stringField(policy.Object, "spec", "workloadRef", key) != value ||
			stringField(request.Object, "spec", "workloadRef", key) != value {
			return nil, fmt.Errorf("selected recovery workload identity mismatch")
		}
	}
	if replicas, present := selected["replicas"]; present && replicas != intField(request.Object, "spec", "groupRestore", "worldSize") {
		return nil, fmt.Errorf("selected recovery requires full world placement")
	}
	if err := validateGroupSourceFence(ctx, reader, request); err != nil {
		return nil, err
	}
	if err := validateGroupVolumes(ctx, reader, binding, request); err != nil {
		return nil, err
	}
	migration := &unstructured.Unstructured{}
	migration.SetGroupVersionKind(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMigration"})
	if err := reader.Get(ctx, client.ObjectKey{Namespace: binding.GetNamespace(), Name: a["migration.dcnlab.com/pv-migration"]}, migration); err != nil {
		return nil, err
	}
	if migration.GetAnnotations()[placementRequestUIDAnnotation] != string(request.GetUID()) {
		return nil, fmt.Errorf("PV migration belongs to another restore request")
	}
	next := binding.DeepCopy()
	annotations := next.GetAnnotations()
	pending, err := json.Marshal(clusters)
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal([]interface{}{map[string]interface{}{"name": source}})
	if err != nil {
		return nil, err
	}
	annotations[pendingPlacementAnnotation] = string(pending)
	annotations[applicationFailoverSourceAnnotation] = string(snapshot)
	delete(annotations, recoverSelectedPlacement)
	next.SetAnnotations(annotations)
	if err := unstructured.SetNestedSlice(next.Object, []interface{}{}, "spec", "clusters"); err != nil {
		return nil, err
	}
	return next, nil
}

func (h *PlacementAdmission) admitSelectedRecovery(ctx context.Context, policy, old, next *unstructured.Unstructured, username string) (bool, error) {
	if old.GetAnnotations()[recoverSelectedPlacement] == "" {
		return false, nil
	}
	// Ordinary status-independent updates may proceed under the existing hold.
	if next.GetAnnotations()[applicationFailoverSourceAnnotation] == "" {
		if err := preserveApplicationFailoverHold(old, next); err != nil {
			return false, err
		}
		if !reflect.DeepEqual(old.Object["spec"], next.Object["spec"]) {
			return false, fmt.Errorf("selected recovery must retain held placement until validated")
		}
		return false, nil
	}
	if h.ReleaseUsername == "" || username != h.ReleaseUsername {
		return false, fmt.Errorf("only the placement controller may recover selected placement")
	}
	if err := sameApplicationFailoverIdentity(old, next); err != nil {
		return false, err
	}
	expected, err := recoverSelectedBinding(ctx, h.Reader, policy, old)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(expected.Object["spec"], next.Object["spec"]) ||
		!reflect.DeepEqual(expected.GetAnnotations(), next.GetAnnotations()) {
		return false, fmt.Errorf("selected recovery must preserve suspension and exact scheduling intent")
	}
	return true, nil
}
