package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A shared checkpoint receipt cannot attest unrelated ordinal data volumes.
func validateGroupVolumes(ctx context.Context, reader client.Reader, binding, request *unstructured.Unstructured) error {
	source, target := stringField(request.Object, "spec", "sourceCluster"), stringField(request.Object, "spec", "targetCluster")
	if source == target {
		return nil
	}
	sts := p.NewObject("StatefulSet")
	if err := reader.Get(ctx, client.ObjectKey{Namespace: request.GetNamespace(), Name: stringField(request.Object, "spec", "workloadRef", "name")}, sts); err != nil {
		return err
	}
	if sts.GetUID() == "" || string(sts.GetUID()) != stringField(request.Object, "spec", "workloadRef", "uid") || !sts.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("volume workload identity mismatch")
	}
	claimNames, err := groupVolumeClaims(sts, request)
	if err != nil {
		return err
	}
	if len(claimNames) == 0 {
		return nil
	}
	a := binding.GetAnnotations()
	name, uid := a["migration.dcnlab.com/pv-migration"], a["migration.dcnlab.com/pv-migration-uid"]
	if name == "" || uid == "" {
		return fmt.Errorf("PVC group restore requires UID-bound PVMigration")
	}
	pv := &unstructured.Unstructured{}
	pv.SetGroupVersionKind(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMigration"})
	if err := reader.Get(ctx, client.ObjectKey{Namespace: request.GetNamespace(), Name: name}, pv); err != nil {
		return err
	}
	if string(pv.GetUID()) != uid || !pv.GetDeletionTimestamp().IsZero() || pv.GetGeneration() <= 0 || intField(pv.Object, "status", "observedGeneration") != pv.GetGeneration() || stringField(pv.Object, "status", "phase") != "Completed" || stringField(pv.Object, "status", "planHash") == "" || !boolField(pv.Object, "spec", "sourceFenced") {
		return fmt.Errorf("current completed PVMigration required")
	}
	if stringField(pv.Object, "spec", "sourceCluster") != source || stringField(pv.Object, "spec", "targetCluster") != target || stringField(pv.Object, "spec", "resourceBinding") != binding.GetName() {
		return fmt.Errorf("PV migration route mismatch")
	}
	md := &unstructured.Unstructured{}
	md.SetGroupVersionKind(pv.GroupVersionKind().GroupVersion().WithKind("PVMetadata"))
	if err := reader.Get(ctx, client.ObjectKey{Namespace: request.GetNamespace(), Name: stringField(pv.Object, "spec", "metadataRef")}, md); err != nil {
		return err
	}
	if !md.GetDeletionTimestamp().IsZero() || stringField(md.Object, "spec", "sourceCluster") != source || stringField(md.Object, "spec", "workloadRef", "name") != sts.GetName() || stringField(md.Object, "spec", "workloadRef", "uid") != string(sts.GetUID()) {
		return fmt.Errorf("PVMetadata workload identity mismatch")
	}
	claims := map[string]bool{}
	for _, claim := range claimNames {
		claims[claim] = true
	}
	volumes, _, err := unstructured.NestedSlice(pv.Object, "spec", "volumes")
	if err != nil || len(claims) == 0 || len(volumes) != len(claims) {
		return fmt.Errorf("PV mapping must cover all workload claims")
	}
	for _, raw := range volumes {
		v, ok := raw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid PV mapping")
		}
		s := stringField(v, "sourcePVC")
		if !claims[s] || stringField(v, "targetPVC") != s {
			return fmt.Errorf("PV mapping changed or duplicated workload claim")
		}
		delete(claims, s)
	}
	works, _, err := unstructured.NestedSlice(pv.Object, "status", "works")
	if err != nil {
		return fmt.Errorf("incomplete PV Work evidence")
	}
	templateOnly := sts.DeepCopy()
	unstructured.RemoveNestedField(templateOnly.Object, "spec", "template", "spec", "volumes")
	unstructured.RemoveNestedField(templateOnly.Object, "spec", "template", "metadata", "annotations", "fluidcr.dcnlab.com/checkpoint-claim")
	templateClaims, err := groupVolumeClaims(templateOnly, request)
	if err != nil {
		return err
	}
	templates := map[string]bool{}
	for _, claim := range templateClaims {
		templates[claim] = true
	}
	expectedCount := len(volumes)
	for _, claim := range claimNames {
		if !templates[claim] {
			expectedCount++
		}
	}
	if len(works) != expectedCount {
		return fmt.Errorf("incomplete split PV/PVC Work evidence")
	}
	seen := map[string]bool{}
	byName := map[string]map[string]interface{}{}
	for _, raw := range works {
		w, ok := raw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid PV Work")
		}
		name := stringField(w, "name")
		if name == "" || seen[name] || stringField(w, "namespace") != "karmada-es-"+target || !boolField(w, "applied") || !boolField(w, "detached") {
			return fmt.Errorf("PV Work not applied and detached")
		}
		seen[name] = true
		byName[name] = w
	}
	for _, claim := range claimNames {
		if templates[claim] {
			continue
		}
		name := groupPVWorkName(string(sts.GetUID()), target, request.GetNamespace(), claim)
		pvWork, pvcWork := byName[name], byName[name+"-pvc"]
		if pvWork == nil || pvcWork == nil || !boolField(pvWork, "standalone") || !boolField(pvcWork, "standalone") || stringField(pvWork, "pvUID") == "" || stringField(pvWork, "pvcUID") == "" || stringField(pvWork, "pvUID") != stringField(pvcWork, "pvUID") || stringField(pvWork, "pvcUID") != stringField(pvcWork, "pvcUID") {
			return fmt.Errorf("standalone claim %s lacks reciprocal split Work receipts", claim)
		}
		delete(byName, name)
		delete(byName, name+"-pvc")
	}
	for _, w := range byName {
		if boolField(w, "standalone") {
			return fmt.Errorf("unexpected standalone Work receipt")
		}
	}
	return nil
}

// Wire identity shared with PV-Migration-System stableName("pvm", ...).
func groupPVWorkName(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		fmt.Fprintf(h, "%d:%s;", len(part), part)
	}
	return "pvm-" + hex.EncodeToString(h.Sum(nil))[:24]
}
