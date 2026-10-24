package management

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ExistingClaimRequestUIDLabel = "training.dcnlab.com/claim-restore-request-uid"

const (
	existingClaimPVOwnerLabel   = "migration.dcnlab.com/migration-uid"
	existingClaimPVManagedLabel = "migration.dcnlab.com/managed-by"
	existingClaimPVManager      = "pv-migration-system"
)

// EnsureExistingTargetClaims is an Existing-capacity-only prerequisite. The caller
// must validate the source fence and validateGroupVolumes before calling it with
// a direct target-member client. It never updates/deletes claims or creates Pods.
func EnsureExistingTargetClaims(ctx context.Context, member client.Client, sts, migration, metadata, request *unstructured.Unstructured) (bool, error) {
	if member == nil || sts == nil || migration == nil || metadata == nil || request == nil {
		return false, fmt.Errorf("target claims require member and complete migration evidence")
	}
	ns := request.GetNamespace()
	for _, obj := range []*unstructured.Unstructured{sts, migration, metadata, request} {
		if obj.GetUID() == "" || obj.GetName() == "" || ns == "" || obj.GetNamespace() != ns || !obj.GetDeletionTimestamp().IsZero() {
			return false, fmt.Errorf("target claim evidence identity/namespace mismatch")
		}
	}
	if sts.GetAPIVersion() != "apps/v1" || sts.GetKind() != "StatefulSet" ||
		stringField(request.Object, "spec", "workloadRef", "name") != sts.GetName() || stringField(request.Object, "spec", "workloadRef", "uid") != string(sts.GetUID()) ||
		stringField(metadata.Object, "spec", "workloadRef", "name") != sts.GetName() || stringField(metadata.Object, "spec", "workloadRef", "uid") != string(sts.GetUID()) {
		return false, fmt.Errorf("target claims require the exact StatefulSet UID")
	}
	source, target := stringField(request.Object, "spec", "sourceCluster"), stringField(request.Object, "spec", "targetCluster")
	if source == "" || target == "" || source == target || stringField(migration.Object, "spec", "sourceCluster") != source || stringField(migration.Object, "spec", "targetCluster") != target ||
		stringField(metadata.Object, "spec", "sourceCluster") != source || stringField(migration.Object, "spec", "metadataRef") != metadata.GetName() ||
		migration.GetAnnotations()[placementRequestUIDAnnotation] != string(request.GetUID()) {
		return false, fmt.Errorf("target claim migration route/request identity mismatch")
	}
	if migration.GetGeneration() <= 0 || intField(migration.Object, "status", "observedGeneration") != migration.GetGeneration() || stringField(migration.Object, "status", "phase") != "Completed" ||
		stringField(migration.Object, "status", "planHash") == "" || !boolField(migration.Object, "spec", "sourceFenced") {
		return false, fmt.Errorf("target claims require current Completed PVMigration")
	}
	var workload appsv1.StatefulSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(sts.Object, &workload); err != nil {
		return false, err
	}
	claims, ordinals, err := existingClaimTemplates(&workload, request)
	if err != nil {
		return false, err
	}
	volumes, err := existingClaimSourceVolumes(metadata, source)
	if err != nil {
		return false, err
	}
	mappings, _, err := unstructured.NestedSlice(migration.Object, "spec", "volumes")
	if err != nil || len(mappings) != len(claims) {
		return false, fmt.Errorf("target claim mappings must cover all VCT claims")
	}
	seen := map[string]bool{}
	sourceSpecs := map[string]corev1.PersistentVolumeSpec{}
	for _, raw := range mappings {
		mapping, ok := raw.(map[string]interface{})
		name := stringField(mapping, "targetPVC")
		template, found := claims[name]
		if !ok || !found || seen[name] || stringField(mapping, "sourcePVC") != name {
			return false, fmt.Errorf("target claim mapping is foreign, duplicated or renamed")
		}
		seen[name] = true
		v := volumes[name]
		if v == nil || stringField(v, "templateName") != template.Name || intField(v, "ordinal") != ordinals[name] || stringField(v, "pvName") == "" || stringField(v, "pvcUID") == "" {
			return false, fmt.Errorf("source volume %s template/ordinal identity mismatch", name)
		}
		specMap, _, err := unstructured.NestedMap(v, "pvSpec")
		if err != nil {
			return false, err
		}
		var spec corev1.PersistentVolumeSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(specMap, &spec); err != nil {
			return false, err
		}
		if spec.NFS == nil || spec.NFS.Server == "" || spec.NFS.Path == "" || spec.NodeAffinity != nil ||
			!equality.Semantic.DeepEqual(spec.PersistentVolumeSource, corev1.PersistentVolumeSource{NFS: spec.NFS}) ||
			spec.ClaimRef == nil || spec.ClaimRef.Namespace != ns || spec.ClaimRef.Name != name || string(spec.ClaimRef.UID) != stringField(v, "pvcUID") {
			return false, fmt.Errorf("source volume %s is not UID-bound portable NFS", name)
		}
		spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		spec.ClaimRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: ns, Name: name}
		sourceSpecs[name] = spec
	}
	works, _, err := unstructured.NestedSlice(migration.Object, "status", "works")
	if err != nil || len(works) != len(claims) {
		return false, fmt.Errorf("target claims require complete Work receipts")
	}
	missing := []*corev1.PersistentVolumeClaim{}
	allBound := true
	seenPV, seenClaim := map[string]bool{}, map[string]bool{}
	// Finish all reads/validation before the first create, including foreign claims.
	for _, raw := range works {
		work, ok := raw.(map[string]interface{})
		name := stringField(work, "name")
		if !ok || name == "" || seenPV[name] || stringField(work, "namespace") != "karmada-es-"+target || !boolField(work, "applied") || !boolField(work, "detached") {
			return false, fmt.Errorf("target PV Work receipt incomplete or duplicated")
		}
		seenPV[name] = true
		var pv corev1.PersistentVolume
		if err := member.Get(ctx, client.ObjectKey{Name: name}, &pv); err != nil {
			return false, err
		}
		if pv.UID == "" || !pv.DeletionTimestamp.IsZero() || pv.Labels[existingClaimPVOwnerLabel] != string(migration.GetUID()) || pv.Labels[existingClaimPVManagedLabel] != existingClaimPVManager || pv.Spec.ClaimRef == nil || (pv.Status.Phase != corev1.VolumeAvailable && pv.Status.Phase != corev1.VolumeBound) {
			return false, fmt.Errorf("target PV %s ownership/identity mismatch", name)
		}
		claimName := pv.Spec.ClaimRef.Name
		expected, found := sourceSpecs[claimName]
		if !found || seenClaim[claimName] {
			return false, fmt.Errorf("target PV %s has foreign/duplicate claim reference", name)
		}
		seenClaim[claimName] = true
		actual := pv.Spec.DeepCopy()
		actual.ClaimRef.UID, actual.ClaimRef.ResourceVersion = "", ""
		actual.VolumeMode = existingClaimVolumeMode(actual.VolumeMode)
		expected.VolumeMode = existingClaimVolumeMode(expected.VolumeMode)
		if !equality.Semantic.DeepEqual(*actual, expected) {
			return false, fmt.Errorf("target PV %s differs from source NFS/spec or claim reference", name)
		}
		desired, err := existingClaimFromTemplate(claims[claimName], &pv, ns, claimName, string(request.GetUID()))
		if err != nil {
			return false, err
		}
		var claim corev1.PersistentVolumeClaim
		err = member.Get(ctx, client.ObjectKeyFromObject(desired), &claim)
		if apierrors.IsNotFound(err) {
			if pv.Spec.ClaimRef.UID != "" || pv.Status.Phase != corev1.VolumeAvailable {
				return false, fmt.Errorf("target PV %s already bound or unavailable", name)
			}
			missing = append(missing, desired)
			allBound = false
			continue
		}
		if err != nil {
			return false, err
		}
		claim.Spec.VolumeMode = existingClaimVolumeMode(claim.Spec.VolumeMode)
		desired.Spec.VolumeMode = existingClaimVolumeMode(desired.Spec.VolumeMode)
		if claim.UID == "" || !claim.DeletionTimestamp.IsZero() || claim.Labels[ExistingClaimRequestUIDLabel] != string(request.GetUID()) || !equality.Semantic.DeepEqual(claim.Spec, desired.Spec) ||
			(pv.Spec.ClaimRef.UID != "" && pv.Spec.ClaimRef.UID != claim.UID) {
			return false, fmt.Errorf("target PVC %s ownership/spec/UID conflict", claimName)
		}
		if claim.Status.Phase == corev1.ClaimBound {
			if pv.Spec.ClaimRef.UID != claim.UID || pv.Status.Phase != corev1.VolumeBound {
				return false, fmt.Errorf("target PVC %s lacks reciprocal Bound PV UID", claimName)
			}
		} else if claim.Status.Phase == "" || claim.Status.Phase == corev1.ClaimPending {
			allBound = false
		} else {
			return false, fmt.Errorf("target PVC %s is not bindable", claimName)
		}
	}
	for _, claim := range missing {
		// A create race must be re-read and ownership-checked on the next call.
		if err := member.Create(ctx, claim); err != nil {
			return false, err
		}
	}
	return allBound, nil
}

func existingClaimTemplates(sts *appsv1.StatefulSet, request *unstructured.Unstructured) (map[string]*corev1.PersistentVolumeClaim, map[string]int64, error) {
	pods, _, err := unstructured.NestedSlice(request.Object, "spec", "pods")
	if err != nil || len(pods) == 0 || len(sts.Spec.VolumeClaimTemplates) == 0 {
		return nil, nil, fmt.Errorf("target claims require explicit pods and VCTs")
	}
	claims, ordinals := map[string]*corev1.PersistentVolumeClaim{}, map[string]int64{}
	for i := range sts.Spec.VolumeClaimTemplates {
		template := &sts.Spec.VolumeClaimTemplates[i]
		if template.Name == "" || template.Spec.VolumeName != "" || template.Spec.DataSource != nil || template.Spec.DataSourceRef != nil {
			return nil, nil, fmt.Errorf("unsupported target claim template")
		}
		for _, raw := range pods {
			pod, ok := raw.(map[string]interface{})
			name := stringField(pod, "sourcePod")
			suffix := strings.TrimPrefix(name, sts.Name+"-")
			ordinal, err := strconv.ParseInt(suffix, 10, 32)
			claim := template.Name + "-" + name
			if !ok || !strings.HasPrefix(name, sts.Name+"-") || err != nil || ordinal < 0 || suffix != strconv.FormatInt(ordinal, 10) || claims[claim] != nil {
				return nil, nil, fmt.Errorf("invalid or duplicate target claim ordinal")
			}
			claims[claim], ordinals[claim] = template, ordinal
		}
	}
	return claims, ordinals, nil
}

func existingClaimSourceVolumes(metadata *unstructured.Unstructured, source string) (map[string]map[string]interface{}, error) {
	clusters, _, err := unstructured.NestedSlice(metadata.Object, "status", "clusters")
	if err != nil {
		return nil, err
	}
	var snapshot map[string]interface{}
	for _, raw := range clusters {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid source snapshot")
		}
		if stringField(entry, "clusterName") == source {
			if snapshot != nil {
				return nil, fmt.Errorf("duplicate source snapshot")
			}
			snapshot = entry
		}
	}
	var collected metav1.Time
	if snapshot == nil || metadata.GetGeneration() <= 0 || intField(snapshot, "observedGeneration") != metadata.GetGeneration() || stringField(snapshot, "workloadUID") == "" ||
		collected.UnmarshalQueryParameter(stringField(snapshot, "collectedAt")) != nil || collected.IsZero() {
		return nil, fmt.Errorf("current collected source snapshot required")
	}
	volumes, _, err := unstructured.NestedSlice(snapshot, "volumes")
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]interface{}{}
	for _, raw := range volumes {
		v, ok := raw.(map[string]interface{})
		name := stringField(v, "pvcName")
		if !ok || name == "" || out[name] != nil {
			return nil, fmt.Errorf("invalid or duplicate source PVC snapshot")
		}
		out[name] = v
	}
	return out, nil
}

func existingClaimVolumeMode(mode *corev1.PersistentVolumeMode) *corev1.PersistentVolumeMode {
	if mode != nil {
		return mode
	}
	filesystem := corev1.PersistentVolumeFilesystem
	return &filesystem
}

func existingClaimFromTemplate(template *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, ns, name, uid string) (*corev1.PersistentVolumeClaim, error) {
	spec := template.Spec.DeepCopy()
	if spec.StorageClassName != nil && *spec.StorageClassName != pv.Spec.StorageClassName {
		return nil, fmt.Errorf("target PVC %s storage class differs from PV", name)
	}
	class := pv.Spec.StorageClassName
	spec.StorageClassName, spec.VolumeName = &class, pv.Name
	if *existingClaimVolumeMode(spec.VolumeMode) != *existingClaimVolumeMode(pv.Spec.VolumeMode) {
		return nil, fmt.Errorf("target PVC %s volume mode differs from PV", name)
	}
	capacity, requested := pv.Spec.Capacity[corev1.ResourceStorage], spec.Resources.Requests[corev1.ResourceStorage]
	if requested.Sign() <= 0 || capacity.Cmp(requested) < 0 || len(spec.AccessModes) == 0 {
		return nil, fmt.Errorf("target PV %s cannot satisfy VCT capacity", pv.Name)
	}
	for _, mode := range spec.AccessModes {
		found := false
		for _, available := range pv.Spec.AccessModes {
			found = found || mode == available
		}
		if !found {
			return nil, fmt.Errorf("target PV %s cannot satisfy VCT access mode", pv.Name)
		}
	}
	selector, err := metav1.LabelSelectorAsSelector(spec.Selector)
	if spec.Selector != nil && (err != nil || !selector.Matches(labels.Set(pv.Labels))) {
		return nil, fmt.Errorf("target PV %s does not match VCT selector", pv.Name)
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{}}, Spec: *spec}
	for key, value := range template.Labels {
		claim.Labels[key] = value
	}
	claim.Labels[ExistingClaimRequestUIDLabel] = uid
	return claim, nil
}
