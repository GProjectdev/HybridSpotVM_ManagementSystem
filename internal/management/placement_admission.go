package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const pendingPlacementAnnotation = "training.dcnlab.com/pending-placement"

// PlacementAdmission keeps the old placement in the scheduler's transaction.
// Watching an already changed placement cannot prevent a target cold start.
type PlacementAdmission struct {
	Reader          client.Reader
	ReleaseUsername string
}

const placementRequestAnnotation = "training.dcnlab.com/placement-restore-request"
const placementRequestUIDAnnotation = "training.dcnlab.com/placement-restore-request-uid"

func (h *PlacementAdmission) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Update || req.SubResource != "" {
		return admission.Allowed("not a placement update")
	}
	old, next := bindingObject(), bindingObject()
	if err := utiljson.Unmarshal(req.OldObject.Raw, &old.Object); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if err := utiljson.Unmarshal(req.Object.Raw, &next.Object); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if stringField(next.Object, "spec", "resource", "kind") != "StatefulSet" || stringField(next.Object, "spec", "resource", "apiVersion") != "apps/v1" {
		return admission.Allowed("not a StatefulSet")
	}
	policies := p.NewList("TrainingPolicy")
	if err := h.Reader.List(ctx, policies, client.InNamespace(req.Namespace)); err != nil {
		return admission.Errored(http.StatusServiceUnavailable, err)
	}
	var policy *unstructured.Unstructured
	for i := range policies.Items {
		candidate := &policies.Items[i]
		if stringField(candidate.Object, "spec", "workloadRef", "uid") == stringField(next.Object, "spec", "resource", "uid") && stringField(candidate.Object, "spec", "workloadRef", "name") == stringField(next.Object, "spec", "resource", "name") {
			if policy != nil {
				return admission.Denied("multiple policies own the placement")
			}
			policy = candidate
		}
	}
	if policy == nil {
		return admission.Allowed("unmanaged workload")
	}
	if old.GetAnnotations()[pendingPlacementAnnotation] != "" && next.GetAnnotations()[pendingPlacementAnnotation] == "" {
		if h.ReleaseUsername == "" || req.UserInfo.Username != h.ReleaseUsername {
			return admission.Denied("only the placement controller may release a pending transition")
		}
		if err := h.validateRelease(ctx, policy, old, next); err != nil {
			return admission.Denied(err.Error())
		}
		return admission.Allowed("UID-bound group restore Prepared; releasing placement")
	}
	if err := holdPlacement(old, next); err != nil {
		return admission.Denied(err.Error())
	}
	encoded, err := json.Marshal(next.Object)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, encoded)
}

func (h *PlacementAdmission) validateRelease(ctx context.Context, policy, old, next *unstructured.Unstructured) error {
	var desired []interface{}
	// Kubernetes integers must remain int64 for exact placement comparison.
	if err := utiljson.Unmarshal([]byte(old.GetAnnotations()[pendingPlacementAnnotation]), &desired); err != nil {
		return err
	}
	actual, _, _ := unstructured.NestedSlice(next.Object, "spec", "clusters")
	if !reflect.DeepEqual(desired, actual) {
		return fmt.Errorf("release differs from pending placement")
	}
	if stringField(old.Object, "spec", "resource", "uid") != stringField(next.Object, "spec", "resource", "uid") {
		return fmt.Errorf("release changed workload UID")
	}
	request := newRestoreRequest()
	name := old.GetAnnotations()[placementRequestAnnotation]
	if name == "" || name != next.GetAnnotations()[placementRequestAnnotation] {
		return fmt.Errorf("bound restore request missing")
	}
	if err := h.Reader.Get(ctx, client.ObjectKey{Namespace: old.GetNamespace(), Name: name}, request); err != nil {
		return err
	}
	if string(request.GetUID()) != old.GetAnnotations()[placementRequestUIDAnnotation] || old.GetAnnotations()[placementRequestUIDAnnotation] != next.GetAnnotations()[placementRequestUIDAnnotation] || request.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) {
		return fmt.Errorf("restore request ownership mismatch")
	}
	if stringField(request.Object, "spec", "workloadRef", "uid") != stringField(old.Object, "spec", "resource", "uid") || len(actual) != 1 {
		return fmt.Errorf("restore workload mismatch")
	}
	if old.GetUID() == "" || stringField(request.Object, "spec", "groupRestore", "operationUID") != string(old.GetUID()) {
		return fmt.Errorf("restore operation is not bound to this ResourceBinding")
	}
	before, _, _ := unstructured.NestedSlice(old.Object, "spec", "clusters")
	if len(before) != 1 {
		return fmt.Errorf("source placement missing")
	}
	source, ok := before[0].(map[string]interface{})
	if !ok || stringField(source, "name") != stringField(request.Object, "spec", "sourceCluster") {
		return fmt.Errorf("restore source cluster mismatch")
	}
	target, ok := actual[0].(map[string]interface{})
	if !ok || stringField(target, "name") != stringField(request.Object, "spec", "targetCluster") {
		return fmt.Errorf("restore target mismatch")
	}
	if err := validateGroupRelease(ctx, h.Reader, request); err != nil {
		return err
	}
	return validateGroupVolumes(ctx, h.Reader, old, request)
}

func holdPlacement(old, next *unstructured.Unstructured) error {
	before, _, _ := unstructured.NestedSlice(old.Object, "spec", "clusters")
	after, _, _ := unstructured.NestedSlice(next.Object, "spec", "clusters")
	pending := old.GetAnnotations()[pendingPlacementAnnotation]
	if len(before) == 0 {
		return nil
	}
	if reflect.DeepEqual(before, after) {
		if pending != "" && next.GetAnnotations()[pendingPlacementAnnotation] != pending {
			return fmt.Errorf("pending placement intent cannot be removed or rewritten")
		}
		return nil
	}
	if stringField(old.Object, "spec", "resource", "uid") != stringField(next.Object, "spec", "resource", "uid") {
		return fmt.Errorf("workload UID changed during placement update")
	}
	if len(before) != 1 || len(after) != 1 {
		return fmt.Errorf("managed DDP requires exactly one source and one target cluster")
	}
	b, bok := before[0].(map[string]interface{})
	a, aok := after[0].(map[string]interface{})
	if !bok || !aok || stringField(a, "name") == "" {
		return fmt.Errorf("invalid placement")
	}
	if stringField(a, "name") == stringField(b, "name") {
		return nil
	}
	raw, err := json.Marshal(after)
	if err != nil {
		return err
	}
	if pending != "" && pending != string(raw) {
		return fmt.Errorf("another cluster transition is already pending")
	}
	annotations := next.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[pendingPlacementAnnotation] = string(raw)
	next.SetAnnotations(annotations)
	return unstructured.SetNestedSlice(next.Object, before, "spec", "clusters")
}
