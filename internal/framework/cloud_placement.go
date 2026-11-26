package framework

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyCloudScheduledNodes(ctx context.Context, members MemberReader, cap *unstructured.Unstructured, o WorkloadObservation, cluster string) error {
	if cap == nil {
		return fmt.Errorf("Cloud allocation receipt unavailable")
	}
	entries, _, _ := unstructured.NestedMap(cap.Object, "status", "allocations")
	byNode := map[string]map[string]interface{}{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := entry["nodeName"].(string)
		if name != "" && entry["phase"] == "Ready" {
			byNode[name] = entry
		}
	}
	reader, err := members(cluster)
	if err != nil {
		return err
	}
	for pod := range o.ScheduledPods {
		nodeName := o.ScheduledNodes[pod]
		entry := byNode[nodeName]
		if entry == nil {
			return fmt.Errorf("Pod %s is not scheduled on an owned Ready GPU node", pod)
		}
		node := &corev1.Node{}
		if err := reader.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			return err
		}
		if string(node.UID) != entry["nodeUID"] || node.Labels["ml.dcn.ssu.ac.kr/node-provision-uid"] != entry["memberUID"] || !node.DeletionTimestamp.IsZero() {
			return fmt.Errorf("Cloud node incarnation changed")
		}
	}
	return nil
}

// Restrict only the Cloud copy to the verified allocation UIDs. Existing user
// node affinity/selector restrictions are retained, not silently weakened.
func ensureCloudNodePlacement(ctx context.Context, c client.Client, run, work, cap *unstructured.Unstructured, s RunSpec) error {
	if str(work, "spec", "template", "spec", "nodeName") != "" {
		return fmt.Errorf("fixed nodeName prevents portable Cloud placement")
	}
	var cs CapacitySpec
	if decodeSpec(cap, &cs) != nil {
		return fmt.Errorf("invalid capacity contract")
	}
	values := []interface{}{}
	for _, a := range cs.Allocations {
		uid := str(cap, "status", "allocations", a.Name, "memberUID")
		if uid == "" || a.State == "Retire" {
			return fmt.Errorf("unverified target allocation")
		}
		values = append(values, uid)
	}
	if int64(len(values)) != s.WorldSize {
		return fmt.Errorf("incomplete target world")
	}
	affinity, _, err := unstructured.NestedMap(work.Object, "spec", "template", "spec", "affinity")
	if err != nil {
		return err
	}
	if affinity == nil {
		affinity = map[string]interface{}{}
	}
	terms, found, err := unstructured.NestedSlice(affinity, "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	if err != nil {
		return err
	}
	if !found {
		terms = []interface{}{map[string]interface{}{}}
	}
	if len(terms) == 0 {
		return fmt.Errorf("empty user required affinity matches no node")
	}
	for _, raw := range terms {
		term, ok := raw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid node affinity")
		}
		expressions, _, err := unstructured.NestedSlice(term, "matchExpressions")
		if err != nil {
			return err
		}
		expressions = append(expressions, map[string]interface{}{"key": "ml.dcn.ssu.ac.kr/node-provision-uid", "operator": "In", "values": values})
		term["matchExpressions"] = expressions
	}
	_ = unstructured.SetNestedSlice(affinity, terms, "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	o := object("policy.karmada.io/v1alpha1", "OverridePolicy")
	o.SetNamespace(run.GetNamespace())
	o.SetName(work.GetName() + "-cloud-nodes")
	o.Object["spec"] = map[string]interface{}{"resourceSelectors": []interface{}{map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": work.GetName()}}, "overrideRules": []interface{}{map[string]interface{}{"targetCluster": map[string]interface{}{"clusterNames": []interface{}{s.Cloud.Cluster}}, "overriders": map[string]interface{}{"plaintext": []interface{}{map[string]interface{}{"path": "/spec/template/spec/affinity", "operator": "add", "value": affinity}}}}}}
	return ensureSpec(ctx, c, o, run)
}

// Credential copies must exist in both initial candidates. Ordered single-group
// placement is suitable for a workload, but would propagate a dependency to only
// one candidate, leaving the other unable to start its execution gate.
func ensureGateDependencyPlacement(ctx context.Context, c client.Client, run, obj *unstructured.Unstructured, s RunSpec) error {
	clusters := []interface{}{s.OnPremiseCluster}
	if s.Cloud.Allowed {
		clusters = append(clusters, s.Cloud.Cluster)
	}
	pp := object("policy.karmada.io/v1alpha1", "PropagationPolicy")
	pp.SetNamespace(obj.GetNamespace())
	pp.SetName(obj.GetName() + "-training-placement")
	pp.Object["spec"] = map[string]interface{}{"conflictResolution": "Abort", "resourceSelectors": []interface{}{map[string]interface{}{"apiVersion": obj.GetAPIVersion(), "kind": obj.GetKind(), "name": obj.GetName()}}, "placement": map[string]interface{}{"clusterAffinity": map[string]interface{}{"clusterNames": clusters}}}
	// Remove only the old mutually-exclusive placement fields owned by this Run.
	old := object(pp.GetAPIVersion(), pp.GetKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(pp), old); err == nil && owned(old, run) {
		if _, found, _ := unstructured.NestedSlice(old.Object, "spec", "placement", "clusterAffinities"); found {
			base := old.DeepCopy()
			old.Object["spec"] = pp.Object["spec"]
			return c.Patch(ctx, old, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}
	}
	return ensureSpec(ctx, c, pp, run)
}

// Request a fresh native scheduling calculation after the Cloud candidate becomes
// usable. Policy edits alone may retain a previously selected, now-full member.
// Never rewrite spec.clusters, and never reschedule an admitted execution.
func requestInitialReschedule(ctx context.Context, c client.Client, run, work *unstructured.Unstructured, s RunSpec, o WorkloadObservation, ready bool, now time.Time) (bool, error) {
	if !s.ExecutionGate || !ready || !boolAt(run, "status", "cloudEligible") || str(run, "status", "committedCluster") != "" || o.AllPodsScheduled || !o.GPUShortage {
		return false, nil
	}
	if _, exists, _ := unstructured.NestedMap(run.Object, "status", "executionPermit"); exists {
		return false, nil
	}
	bindings, err := listItems(ctx, c, "work.karmada.io/v1alpha2", "ResourceBinding", run.GetNamespace())
	if err != nil {
		return false, err
	}
	for i := range bindings {
		b := &bindings[i]
		if str(b, "spec", "resource", "uid") != string(work.GetUID()) {
			continue
		}
		if !b.GetDeletionTimestamp().IsZero() || str(b, "spec", "resource", "name") != work.GetName() || str(b, "spec", "resource", "kind") != "StatefulSet" {
			return false, fmt.Errorf("binding identity changed")
		}
		groups, _, _ := unstructured.NestedSlice(b.Object, "spec", "placement", "clusterAffinities")
		allowed := false
		for _, raw := range groups {
			g, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			names, _, _ := unstructured.NestedStringSlice(g, "clusterNames")
			for _, n := range names {
				if n == s.Cloud.Cluster {
					allowed = true
				}
			}
		}
		if !allowed {
			return false, nil
		} // wait for Karmada to observe the policy update
		if at, e := time.Parse(time.RFC3339Nano, str(b, "spec", "rescheduleTriggeredAt")); e == nil && now.Sub(at) < time.Minute {
			return false, nil
		}
		base := b.DeepCopy()
		_ = unstructured.SetNestedField(b.Object, now.UTC().Format(time.RFC3339), "spec", "rescheduleTriggeredAt")
		return true, c.Patch(ctx, b, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	return false, nil
}
