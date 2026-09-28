package management

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const groupFenceAnnotation = "migration.dcnlab.com/group-source-fence"

// validateGroupRelease reads controller evidence rather than trusting the
// request's phase alone. It never changes either the request or the plan.
func validateGroupRelease(ctx context.Context, reader client.Reader, request *unstructured.Unstructured) error {
	if request.GetUID() == "" || request.GetGeneration() < 1 || !request.GetDeletionTimestamp().IsZero() || intField(request.Object, "status", "observedGeneration") != request.GetGeneration() {
		return fmt.Errorf("restore request identity or observed generation is stale")
	}
	switch stringField(request.Object, "status", "phase") {
	case "Prepared", "Running", "Verified":
	default:
		return fmt.Errorf("restore request is not prepared")
	}
	name := stringField(request.Object, "status", "planName")
	if name == "" {
		return fmt.Errorf("restore plan name missing")
	}
	plan := &unstructured.Unstructured{}
	plan.SetGroupVersionKind(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "RestorePlan"})
	if err := reader.Get(ctx, client.ObjectKey{Namespace: request.GetNamespace(), Name: name}, plan); err != nil {
		return err
	}
	return validateGroupPlanRelease(request, plan)
}

func validateGroupPlanRelease(request, plan *unstructured.Unstructured) error {
	if plan.GetName() != stringField(request.Object, "status", "planName") || plan.GetNamespace() != request.GetNamespace() || plan.GetUID() == "" || plan.GetGeneration() < 1 || !plan.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("restore plan identity mismatch")
	}
	owned := false
	for _, owner := range plan.GetOwnerReferences() {
		if owner.Controller != nil && *owner.Controller && owner.APIVersion == "migration.dcnlab.com/v1alpha1" && owner.Kind == "RestoreRequest" && owner.Name == request.GetName() && owner.UID == request.GetUID() {
			owned = true
		}
	}
	if !owned {
		return fmt.Errorf("restore plan controller owner mismatch")
	}
	expected, _, _ := unstructured.NestedMap(request.Object, "spec")
	actual, _, _ := unstructured.NestedMap(plan.Object, "spec")
	if expected == nil || actual == nil {
		return fmt.Errorf("restore spec missing")
	}
	expected["requestUID"] = string(request.GetUID())
	// The typed Plan API emits this default even when the RR omitted it.
	if _, ok := expected["localPodRestore"]; !ok {
		expected["localPodRestore"] = false
	}
	if _, ok := actual["localPodRestore"]; !ok {
		actual["localPodRestore"] = false
	}
 for _,spec:=range []map[string]interface{}{expected,actual}{
  pods,_,_:=unstructured.NestedSlice(spec,"pods")
  for _,raw:=range pods{
   if pod,ok:=raw.(map[string]interface{});ok {if _,exists:=pod["rank"];!exists{pod["rank"]=int64(0)}}
  }
  if pods!=nil{spec["pods"]=pods}
 }
	if !reflect.DeepEqual(expected, actual) {
		return fmt.Errorf("restore plan spec differs from immutable request")
	}
	group, ok, _ := unstructured.NestedMap(expected, "groupRestore")
	if !ok || stringField(group, "operationUID") == "" || stringField(group, "sourceWorldUID") != stringField(expected, "workloadRef", "uid") {
		return fmt.Errorf("group identity missing or mismatched")
	}
	targetName := stringField(expected, "targetCluster")
	var target map[string]interface{}
	matches := 0
	for _, entry := range nestedClusterStatuses(plan.Object) {
		if stringField(entry, "clusterName") != targetName {
			continue
		}
		matches++
		if matches > 1 {
			return fmt.Errorf("duplicate target report")
		}
		target = entry
	}
	if target == nil || intField(target, "observedGeneration") != plan.GetGeneration() {
		return fmt.Errorf("target report is missing or stale")
	}
	if stringField(target, "phase") != "Prepared" {
		return fmt.Errorf("target is not in Prepared release phase")
	}
	gc, _, _ := unstructured.NestedMap(target, "groupControl")
	if stringField(gc, "operationUID") != stringField(group, "operationUID") || stringField(gc, "checkpointID") != stringField(expected, "checkpointRef", "checkpointID") || intField(gc, "checkpointGeneration") <= 0 {
		return fmt.Errorf("prepared checkpoint identity mismatch")
	}
	for _, key := range []string{"prepareJobUID", "volumeServer", "volumePath", "volumePVCUID", "volumePVUID"} {
		if stringField(gc, key) == "" {
			return fmt.Errorf("prepared %s missing", key)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, stringField(gc, "preparedAt")); err != nil {
		return fmt.Errorf("prepare completion timestamp missing")
	}
	mirrored, _, _ := unstructured.NestedMap(request.Object, "status", "groupControl")
	if !reflect.DeepEqual(gc, mirrored) {
		return fmt.Errorf("request group control does not match target evidence")
	}
	var fences []interface{}
	if stringField(expected, "sourceCluster") == targetName {
		fences, _, _ = unstructured.NestedSlice(target, "sourceFences")
	} else {
		var receipt map[string]interface{}
		if err := json.Unmarshal([]byte(plan.GetAnnotations()[groupFenceAnnotation]), &receipt); err != nil {
			return fmt.Errorf("management source fence receipt missing")
		}
		for key, want := range map[string]string{"requestUID": string(request.GetUID()), "operationUID": stringField(group, "operationUID"), "sourceWorldUID": stringField(group, "sourceWorldUID"), "sourceCluster": stringField(expected, "sourceCluster"), "volumeServer": stringField(gc, "volumeServer"), "volumePath": stringField(gc, "volumePath")} {
			if stringField(receipt, key) != want {
				return fmt.Errorf("source receipt %s mismatch", key)
			}
		}
		fences, _ = receipt["fences"].([]interface{})
	}
	sources, _, _ := unstructured.NestedSlice(group, "sourcePods")
	if len(sources) == 0 || int64(len(sources)) != intField(group, "worldSize") || len(fences) != len(sources) {
		return fmt.Errorf("incomplete source world fencing")
	}
	seen := map[string]bool{}
	for _, raw := range sources {
		src, ok := raw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid source identity")
		}
		name, uid := stringField(src, "podName"), stringField(src, "podUID")
		if name == "" || uid == "" || seen[name] {
			return fmt.Errorf("duplicate or empty source identity")
		}
		seen[name] = true
		matches := 0
		for _, rawFence := range fences {
			f, ok := rawFence.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid fence")
			}
			if stringField(f, "podName") != name {
				continue
			}
			matches++
			// JSON receipt numbers are decoded as float64; aggregate status uses int64.
			gen := int64(0)
			switch v := f["observedGeneration"].(type) {
			case int64:
				gen = v
			case float64:
				if v == float64(int64(v)) {
					gen = int64(v)
				}
			}
			if stringField(f, "sourcePodUID") != uid || stringField(f, "phase") != "SourceGone" || gen != plan.GetGeneration() {
				return fmt.Errorf("source fence UID or generation mismatch")
			}
			deleted, e1 := time.Parse(time.RFC3339Nano, stringField(f, "deleteRequestedAt"))
			gone, e2 := time.Parse(time.RFC3339Nano, stringField(f, "goneObservedAt"))
			if e1 != nil || e2 != nil || gone.Before(deleted) {
				return fmt.Errorf("source fence timestamps invalid")
			}
		}
		if matches != 1 {
			return fmt.Errorf("source fence missing or duplicated")
		}
	}
	return nil
}
