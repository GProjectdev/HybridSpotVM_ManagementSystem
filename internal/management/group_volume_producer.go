package management

import (
	"context"
	"fmt"
	"reflect"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const groupVolumeHold = "migration.dcnlab.com/group-volume-hold"

// Called only after validating Prepared and the exact full-world fence receipt.
func (r *GroupPlacementReconciler) ensureGroupVolumes(ctx context.Context, binding, request *unstructured.Unstructured) (bool, error) {
	sts := p.NewObject("StatefulSet")
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: request.GetNamespace(), Name: stringField(request.Object, "spec", "workloadRef", "name")}, sts); err != nil {
		return false, err
	}
	if string(sts.GetUID()) != stringField(request.Object, "spec", "workloadRef", "uid") {
		return false, fmt.Errorf("volume workload UID changed")
	}
	templates, _, err := unstructured.NestedSlice(sts.Object, "spec", "volumeClaimTemplates")
	if err != nil {
		return false, err
	}
	if len(templates) == 0 {
		return true, nil
	}
	source, target := stringField(request.Object, "spec", "sourceCluster"), stringField(request.Object, "spec", "targetCluster")
	if source == target {
		return true, nil
	}
	a := binding.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	if a[groupVolumeHold] != string(request.GetUID()) {
		if a[groupVolumeHold] != "" || boolField(binding.Object, "spec", "suspension", "dispatching") {
			return false, fmt.Errorf("dispatch suspension owned by another operation")
		}
		before := binding.DeepCopy()
		a[groupVolumeHold] = string(request.GetUID())
		binding.SetAnnotations(a)
		_ = unstructured.SetNestedField(binding.Object, true, "spec", "suspension", "dispatching")
		return false, r.Patch(ctx, binding, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if !boolField(binding.Object, "spec", "suspension", "dispatching") {
		return false, fmt.Errorf("group volume dispatch hold removed")
	}
	if a["migration.dcnlab.com/pv-migration"] != "" {
		return true, nil
	}
	metadata := &unstructured.UnstructuredList{}
	metadata.SetGroupVersionKind(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMetadataList"})
	if err := r.Reader.List(ctx, metadata, client.InNamespace(request.GetNamespace())); err != nil {
		return false, err
	}
	var md *unstructured.Unstructured
	for i := range metadata.Items {
		item := &metadata.Items[i]
		if stringField(item.Object, "spec", "sourceCluster") == source && stringField(item.Object, "spec", "workloadRef", "uid") == string(sts.GetUID()) && stringField(item.Object, "spec", "workloadRef", "name") == sts.GetName() && item.GetDeletionTimestamp().IsZero() {
			if md != nil {
				return false, fmt.Errorf("ambiguous source PVMetadata")
			}
			md = item
		}
	}
	if md == nil {
		return false, fmt.Errorf("waiting for source PVMetadata")
	}
	pods, _, err := unstructured.NestedSlice(request.Object, "spec", "pods")
	if err != nil {
		return false, err
	}
	volumes := []interface{}{}
	for _, raw := range templates {
		t, ok := raw.(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("invalid template")
		}
		for _, rawPod := range pods {
			pod, ok := rawPod.(map[string]interface{})
			if !ok {
				return false, fmt.Errorf("invalid pod")
			}
			claim := stringField(t, "metadata", "name") + "-" + stringField(pod, "sourcePod")
			volumes = append(volumes, map[string]interface{}{"sourcePVC": claim, "targetPVC": claim})
		}
	}
	migration := &unstructured.Unstructured{}
	migration.SetGroupVersionKind(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMigration"})
	migration.SetNamespace(request.GetNamespace())
	migration.SetName("group-pv-" + string(request.GetUID()))
	spec := map[string]interface{}{"sourceCluster": source, "targetCluster": target, "metadataRef": md.GetName(), "resourceBinding": binding.GetName(), "sourceFenced": true, "volumes": volumes}
	err = r.Reader.Get(ctx, client.ObjectKeyFromObject(migration), migration)
	if apierrors.IsNotFound(err) {
		migration.Object["spec"] = spec
		migration.SetAnnotations(map[string]string{placementRequestUIDAnnotation: string(request.GetUID())})
		return false, r.Create(ctx, migration)
	}
	if err != nil {
		return false, err
	}
	actual, _, _ := unstructured.NestedMap(migration.Object, "spec")
	if migration.GetUID() == "" || !migration.GetDeletionTimestamp().IsZero() || migration.GetAnnotations()[placementRequestUIDAnnotation] != string(request.GetUID()) || !reflect.DeepEqual(actual, spec) {
		return false, fmt.Errorf("group PVMigration identity/spec conflict")
	}
	before := binding.DeepCopy()
	a["migration.dcnlab.com/pv-migration"] = migration.GetName()
	a["migration.dcnlab.com/pv-migration-uid"] = string(migration.GetUID())
	binding.SetAnnotations(a)
	return false, r.Patch(ctx, binding, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
