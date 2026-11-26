package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const archiveOrigin = "training.dcnlab.com/archive-origin"

// archiveCheckpoint retains independently observed member-local child evidence
// in the management API before a member becomes unreachable. The copy is never
// propagated: it is an immutable receipt, not another checkpoint command.
func archiveCheckpoint(ctx context.Context, control client.Client, member client.Reader, run, work, schedule *unstructured.Unstructured, cluster string, world int64, now time.Time) (*unstructured.Unstructured, error) {
	if schedule.GetUID() == "" || !owned(schedule, run) || cluster == "" {
		return nil, fmt.Errorf("owned schedule and source cluster required")
	}
	parent := p.NewObject("FluidCRMigration")
	if err := member.Get(ctx, client.ObjectKeyFromObject(schedule), parent); err != nil {
		return nil, err
	}
	if parent.GetUID() == "" || parent.GetAnnotations()["resourcetemplate.karmada.io/uid"] != string(schedule.GetUID()) || parent.GetLabels()[RunUIDLabel] != string(run.GetUID()) || str(parent, "spec", "workloadRef", "uid") != string(work.GetUID()) {
		return nil, fmt.Errorf("member parent provenance mismatch")
	}
	childName, childUID := str(parent, "status", "lastSuccessfulCheckpoint", "name"), str(parent, "status", "lastSuccessfulCheckpoint", "uid")
	if childName == "" || childUID == "" {
		return nil, fmt.Errorf("no durable child available yet")
	}
	child := p.NewObject("FluidCRMigration")
	if err := member.Get(ctx, key(run.GetNamespace(), childName), child); err != nil {
		return nil, err
	}
	if string(child.GetUID()) != childUID || child.GetAnnotations()["training.dcnlab.com/scheduled-parent-uid"] != string(parent.GetUID()) || child.GetGeneration() < 1 || integer(child, "status", "observedGeneration") != child.GetGeneration() || !child.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("checkpoint child incarnation mismatch")
	}
	snapshot, found, _ := unstructured.NestedMap(child.Object, "status")
	if !found || str(child, "status", "phase") != "Completed" {
		return nil, fmt.Errorf("checkpoint not completed")
	}
	spec, _, _ := unstructured.NestedMap(child.Object, "spec")
	if _, scheduled := spec["schedule"]; scheduled {
		return nil, fmt.Errorf("a schedule cannot be an archive")
	}
	proof := map[string]interface{}{"cluster": cluster, "parentName": parent.GetName(), "parentUID": string(parent.GetUID()), "scheduleUID": string(schedule.GetUID()), "childName": childName, "childUID": childUID, "childGeneration": child.GetGeneration(), "childResourceVersion": child.GetResourceVersion(), "spec": spec, "status": snapshot}
	body, err := json.Marshal(proof)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	receipt := p.NewObject("FluidCRMigration")
	receipt.SetNamespace(run.GetNamespace())
	receipt.SetName("archive-" + childUID)
	receipt.SetLabels(map[string]string{RunUIDLabel: string(run.GetUID()), p.LabelPolicyUID: string(run.GetUID()), p.LabelManagedBy: FrameworkManagedBy, p.LabelRole: "checkpoint-archive"})
	receipt.SetAnnotations(map[string]string{"training.dcnlab.com/checkpoint-id": child.GetAnnotations()["training.dcnlab.com/checkpoint-id"], archiveOrigin: string(body), "training.dcnlab.com/archive-evidence-sha256": hex.EncodeToString(sum[:])})
	setOwner(receipt, run)
	receipt.Object["spec"] = spec
	// Validate the original child and complete export receipts before creating any
	// object. The temporary identity is solely for pure validation, never submitted.
	candidate := receipt.DeepCopy()
	candidate.SetUID(child.GetUID())
	candidate.SetGeneration(child.GetGeneration())
	snapshot = runtime.DeepCopyJSONValue(snapshot).(map[string]interface{})
	snapshot["clusterName"] = cluster
	candidate.Object["status"] = map[string]interface{}{"clusters": []interface{}{snapshot}}
	input := p.PolicyInput{Namespace: run.GetNamespace(), PolicyUID: run.GetUID(), SourceCluster: cluster, TargetWorkers: world, WorkloadRef: p.WorkloadRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: work.GetName(), UID: work.GetUID()}}
	_, completed, err := management.ValidateGroupCheckpointEvidence(candidate, input)
	if err != nil {
		return nil, err
	}
	if completed.After(now) {
		return nil, fmt.Errorf("future checkpoint completion")
	}
	pods, _, _ := unstructured.NestedSlice(child.Object, "status", "pods")
	for _, raw := range pods {
		pod := raw.(map[string]interface{})
		files, _, _ := unstructured.NestedSlice(pod, "checkpointFiles")
		for _, f := range files {
			at, e := time.Parse(time.RFC3339, str(&unstructured.Unstructured{Object: f.(map[string]interface{})}, "exportedAt"))
			if e != nil || at.After(now) {
				return nil, fmt.Errorf("invalid archive export time")
			}
		}
	}
	existing := p.NewObject("FluidCRMigration")
	if err = control.Get(ctx, client.ObjectKeyFromObject(receipt), existing); apierrors.IsNotFound(err) {
		if err = control.Create(ctx, receipt); err != nil {
			return nil, err
		}
		if err = control.Get(ctx, client.ObjectKeyFromObject(receipt), existing); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if !owned(existing, run) || !existing.GetDeletionTimestamp().IsZero() || !reflect.DeepEqual(existing.Object["spec"], spec) {
		return nil, fmt.Errorf("archive ownership or immutable spec conflict")
	}
	// Source resourceVersion can advance after completion; immutable archive
	// contents, child UID/generation, round and export evidence must not change.
	var prior map[string]interface{}
	if json.Unmarshal([]byte(existing.GetAnnotations()[archiveOrigin]), &prior) != nil {
		return nil, fmt.Errorf("archive origin receipt missing")
	}
	delete(proof, "childResourceVersion")
	delete(prior, "childResourceVersion")
	a, _ := json.Marshal(proof)
	b, _ := json.Marshal(prior)
	if string(a) != string(b) {
		return nil, fmt.Errorf("completed archive evidence changed")
	}
	snapshot["observedGeneration"] = existing.GetGeneration()
	desiredStatus := map[string]interface{}{"clusters": []interface{}{snapshot}}
	if existing.Object["status"] != nil && !reflect.DeepEqual(existing.Object["status"], desiredStatus) {
		return nil, fmt.Errorf("archive status differs from verified original evidence")
	}
	if existing.Object["status"] == nil {
		if err = resource.SetStatus(ctx, control, existing, desiredStatus); err != nil {
			return nil, err
		}
	}
	return existing, nil
}
