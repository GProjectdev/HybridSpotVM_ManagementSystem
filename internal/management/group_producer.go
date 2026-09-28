package management

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"sort"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const groupRestoreRole = "group-restore"
const groupReplacementRole = "group-replacement"
const groupOldName = "training.dcnlab.com/group-old-nodeprovision"
const groupOldUID = "training.dcnlab.com/group-old-nodeprovision-uid"
const groupNewName = "training.dcnlab.com/group-new-nodeprovision"
const groupNewUID = "training.dcnlab.com/group-new-nodeprovision-uid"
const groupIntentAnnotation = "training.dcnlab.com/group-restore-intent"

func (r *PolicyReconciler) quiesceGroup(ctx context.Context, policy *unstructured.Unstructured, operation string) (bool, error) {
	a := policy.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	if current := a[groupIntentAnnotation]; current != "" && current != operation {
		return false, fmt.Errorf("another group operation owns checkpoint quiescence")
	}
	if a[groupIntentAnnotation] == "" {
		before := policy.DeepCopy()
		a[groupIntentAnnotation] = operation
		policy.SetAnnotations(a)
		return false, r.Patch(ctx, policy, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	return boolField(policy.Object, "status", "checkpoint", "periodicQuiesced") && stringField(policy.Object, "status", "checkpoint", "replacementOperation") == operation && stringField(policy.Object, "status", "checkpoint", "reason") == "group_intent_quiesced", nil
}

func (r *PolicyReconciler) ensureGroupReplacement(ctx context.Context, policy *unstructured.Unstructured, input p.PolicyInput, old *unstructured.Unstructured, operation, replacementName, market, event string) (bool, error) {
	name := operation + "-group-restore"
	existing := newRestoreRequest()
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: name}, existing); err == nil {
		if existing.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) || existing.GetAnnotations()[groupOldUID] != string(old.GetUID()) {
			return false, fmt.Errorf("group restore request collision")
		}
		if event != "" && existing.GetAnnotations()[annotationEmergencyEventID] == "" {
			before := existing.DeepCopy()
			a := existing.GetAnnotations()
			a[annotationEmergencyEventID] = event
			existing.SetAnnotations(a)
			return false, r.Patch(ctx, existing, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	// No instance is created unless the complete recoverable round and current
	// world identities are available independently of runtime health.
	round, err := selectGroupCheckpoint(ctx, r.reader(), input)
	if err != nil {
		return false, err
	}
	source, err := r.groupSourcePods(ctx, input)
	if err != nil {
		return false, err
	}
	pvc, root, err := r.groupCheckpointVolume(ctx, input)
	if err != nil {
		return false, err
	}
	if ready, err := r.quiesceGroup(ctx, policy, string(old.GetUID())); err != nil {
		return false, err
	} else if !ready {
		return false, nil
	}
	replacement := p.NewObject("NodeProvision")
	key := client.ObjectKey{Namespace: input.Namespace, Name: replacementName}
	if err := r.reader().Get(ctx, key, replacement); apierrors.IsNotFound(err) {
		replacement.SetNamespace(key.Namespace)
		replacement.SetName(key.Name)
		replacement.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID), p.LabelPolicy: input.PolicyName, p.LabelManagedBy: "hybridspotvm-system", p.LabelRole: groupReplacementRole})
		replacement.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": operation, groupOldUID: string(old.GetUID())})
		spec, ok, _ := unstructured.NestedMap(old.Object, "spec")
		if !ok {
			return false, fmt.Errorf("missing source NodeProvision spec")
		}
		delete(spec, "fence")
		spec["marketType"] = market
		spec["hostname"] = replacementName
		replacement.Object["spec"] = spec
		if err := r.Create(ctx, replacement); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	if replacement.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) || replacement.GetAnnotations()[groupOldUID] != string(old.GetUID()) || stringField(replacement.Object, "spec", "marketType") != market {
		return false, fmt.Errorf("group replacement identity mismatch")
	}
	if err := r.createIfMissing(ctx, p.NewPropagationPolicyFor(input, replacement, input.Capacity.AWSCluster)); err != nil {
		return false, err
	}
	if replacement.GetUID() == "" || stringField(replacement.Object, "status", "phase") != "Ready" || stringField(replacement.Object, "status", "nodeName") == "" {
		return false, fmt.Errorf("waiting for group replacement NodeProvision %s", replacementName)
	}
	nodes := map[string]string{}
	for _, raw := range source {
		pod := raw.(map[string]interface{})
		node := stringField(pod, "nodeName")
		nodes[node] = node
	}
	oldNode := stringField(old.Object, "status", "nodeName")
	if nodes[oldNode] == "" {
		return false, fmt.Errorf("old NodeProvision no longer hosts a source rank")
	}
	nodes[oldNode] = stringField(replacement.Object, "status", "nodeName")
	req, err := r.newGroupRequest(ctx, input, name, string(old.GetUID()), input.SourceCluster, round, source, pvc, root, nodes)
	if err != nil {
		return false, err
	}
	a := req.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[groupOldName] = old.GetName()
	a[groupOldUID] = string(old.GetUID())
	a[groupNewName] = replacement.GetName()
	a[groupNewUID] = string(replacement.GetUID())
	if event != "" {
		a[annotationEmergencyEventID] = event
	}
	req.SetAnnotations(a)
	if err := r.Create(ctx, req); err != nil {
		return false, err
	}
	return true, nil
}

func (r *PolicyReconciler) groupSourcePods(ctx context.Context, input p.PolicyInput) ([]interface{}, error) {
	rt := p.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.RuntimeRefName}, rt); err != nil {
		return nil, err
	}
	if rt.GetUID() == "" || stringField(rt.Object, "spec", "workloadRef", "uid") != string(input.WorkloadRef.UID) {
		return nil, fmt.Errorf("runtime source world identity mismatch")
	}
	var snapshot []interface{}
	reports := 0
	for _, cluster := range nestedClusterStatuses(rt.Object) {
		if stringField(cluster, "clusterName") != input.SourceCluster {
			continue
		}
		reports++
		if reports > 1 || intField(cluster, "status", "observedGeneration") != rt.GetGeneration() {
			return nil, fmt.Errorf("source snapshot report duplicated or stale generation")
		}
		if stringField(cluster, "status", "sourceWorldUID") != string(input.WorkloadRef.UID) {
			return nil, fmt.Errorf("source identity snapshot world mismatch")
		}
		snapshot, _, _ = unstructured.NestedSlice(cluster, "status", "sourcePods")
	}
	if int64(len(snapshot)) != input.TargetWorkers {
		return nil, fmt.Errorf("complete current-world Pod identity snapshot required")
	}
	nodes := p.NewList("NodeProvision")
	if input.SourceCluster == input.Capacity.AWSCluster {
		if err := r.reader().List(ctx, nodes, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID)}); err != nil {
			return nil, err
		}
	}
	byNode := map[string]*unstructured.Unstructured{}
	for i := range nodes.Items {
		np := &nodes.Items[i]
		name := stringField(np.Object, "status", "nodeName")
		if name != "" {
			if byNode[name] != nil {
				return nil, fmt.Errorf("ambiguous source node ownership")
			}
			byNode[name] = np
		}
	}
	out := make([]interface{}, 0, len(snapshot))
	seen := map[int64]bool{}
	for _, raw := range snapshot {
		s, ok := raw.(map[string]interface{})
		rank := intField(s, "rank")
		if !ok || rank < 0 || rank >= input.TargetWorkers || seen[rank] || stringField(s, "name") != fmt.Sprintf("%s-%d", input.WorkloadRef.Name, rank) || stringField(s, "uid") == "" || stringField(s, "nodeName") == "" {
			return nil, fmt.Errorf("invalid source Pod snapshot")
		}
		seen[rank] = true
		item := map[string]interface{}{"rank": rank, "podName": stringField(s, "name"), "podUID": stringField(s, "uid"), "nodeName": stringField(s, "nodeName")}
		if np := byNode[stringField(s, "nodeName")]; np != nil {
			uid, instance := stringField(np.Object, "status", "memberUID"), stringField(np.Object, "status", "instanceId")
			if uid == "" || instance == "" {
				return nil, fmt.Errorf("member NodeProvision UID and instance identity required")
			}
			item["nodeProvisionRef"] = map[string]interface{}{"name": np.GetName(), "uid": uid, "instanceID": instance}
		} else if input.SourceCluster == input.Capacity.AWSCluster {
			return nil, fmt.Errorf("source node is not owned by policy")
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return intField(out[i].(map[string]interface{}), "rank") < intField(out[j].(map[string]interface{}), "rank")
	})
	return out, nil
}

func (r *PolicyReconciler) groupCheckpointVolume(ctx context.Context, input p.PolicyInput) (string, string, error) {
	sts := p.NewObject("StatefulSet")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.WorkloadRef.Name}, sts); err != nil {
		return "", "", err
	}
	if sts.GetUID() != input.WorkloadRef.UID {
		return "", "", fmt.Errorf("workload UID changed")
	}
	cs, _, _ := unstructured.NestedSlice(sts.Object, "spec", "template", "spec", "containers")
	if len(cs) != 1 {
		return "", "", fmt.Errorf("one application container required")
	}
	c, ok := cs[0].(map[string]interface{})
	if !ok {
		return "", "", fmt.Errorf("invalid container")
	}
	root := "/checkpoint"
	env, _, _ := unstructured.NestedSlice(c, "env")
	for _, raw := range env {
		e := raw.(map[string]interface{})
		switch stringField(e, "name") {
		case "FLUIDCR_CHECKPOINT_DIR":
			if stringField(e, "value") == "" {
				return "", "", fmt.Errorf("literal checkpoint root required")
			}
			root = stringField(e, "value")
		case "FLUIDCR_CHECKPOINT_PATH":
			if root == "/checkpoint" && stringField(e, "value") != "" {
				root = path.Dir(stringField(e, "value"))
			}
		}
	}
	if root != "/checkpoint" {
		return "", "", fmt.Errorf("group restore requires checkpoint root /checkpoint")
	}
	mounts, _, _ := unstructured.NestedSlice(c, "volumeMounts")
	volume := ""
	for _, raw := range mounts {
		m := raw.(map[string]interface{})
		if stringField(m, "mountPath") == root {
			if stringField(m, "subPath") != "" || stringField(m, "subPathExpr") != "" {
				return "", "", fmt.Errorf("whole shared checkpoint PVC required, not subPath")
			}
			volume = stringField(m, "name")
		}
	}
	volumes, _, _ := unstructured.NestedSlice(sts.Object, "spec", "template", "spec", "volumes")
	for _, raw := range volumes {
		v := raw.(map[string]interface{})
		if stringField(v, "name") == volume && volume != "" {
			pvc := stringField(v, "persistentVolumeClaim", "claimName")
			if pvc != "" {
				return pvc, root, nil
			}
		}
	}
	return "", "", fmt.Errorf("shared checkpoint PVC mount at %s required", root)
}

func (r *PolicyReconciler) newGroupRequest(ctx context.Context, input p.PolicyInput, name, operation, target string, round *groupCheckpoint, source []interface{}, pvc, root string, targetNodes map[string]string) (*unstructured.Unstructured, error) {
	rt := p.NewObject("TrainingRuntime")
	runtimeNameValue := input.RuntimeRefName
	if target != input.SourceCluster {
		runtimeNameValue = runtimeName(input.PolicyName, target)
	}
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: runtimeNameValue}, rt); err != nil {
		return nil, err
	}
	if rt.GetUID() == "" || stringField(rt.Object, "spec", "workloadRef", "uid") != string(input.WorkloadRef.UID) {
		return nil, fmt.Errorf("target runtime identity required")
	}
	sourceByRank := map[int64]map[string]interface{}{}
	for _, raw := range source {
		s := raw.(map[string]interface{})
		sourceByRank[intField(s, "rank")] = s
	}
	pods := make([]interface{}, 0, len(round.Pods))
	for _, raw := range round.Pods {
		pod := raw.(map[string]interface{})
		s := sourceByRank[intField(pod, "rank")]
		node := targetNodes[stringField(s, "podName")]
		if node == "" {
			node = targetNodes[stringField(s, "nodeName")]
		}
		if node == "" {
			return nil, fmt.Errorf("target node mapping missing")
		}
		pod["targetNode"] = node
		// Archive provenance remains the checkpoint Pod, not the current fenced Pod.
		pods = append(pods, pod)
	}
	req := newRestoreRequest()
	req.SetNamespace(input.Namespace)
	req.SetName(name)
	req.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID), p.LabelPolicy: input.PolicyName, p.LabelManagedBy: "hybridspotvm-system", p.LabelRole: groupRestoreRole})
	req.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": operation})
	cp := round.Object
	req.Object["spec"] = map[string]interface{}{
		"checkpointRef":      map[string]interface{}{"name": cp.GetName(), "uid": string(cp.GetUID()), "generation": cp.GetGeneration(), "checkpointID": cp.GetAnnotations()["training.dcnlab.com/checkpoint-id"]},
		"workloadRef":        map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": input.WorkloadRef.Name, "uid": string(input.WorkloadRef.UID)},
		"trainingRuntimeRef": map[string]interface{}{"name": rt.GetName(), "uid": string(rt.GetUID())},
		"sourceCluster":      input.SourceCluster, "targetCluster": target, "sourceFenced": false, "volumesReady": true, "pods": pods,
		"groupRestore": map[string]interface{}{"operationUID": operation, "sourceWorldUID": string(input.WorkloadRef.UID), "worldSize": input.TargetWorkers, "sharedPVC": pvc, "checkpointRoot": root, "sourcePods": source},
	}
	return req, nil
}

func activeGroupRestore(ctx context.Context, reader client.Reader, input p.PolicyInput) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(newRestoreRequest().GroupVersionKind().GroupVersion().WithKind("RestoreRequestList"))
	if err := reader.List(ctx, list, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID), p.LabelRole: groupRestoreRole}); err != nil {
		return nil, err
	}
	for i := range list.Items {
		o := &list.Items[i]
		if stringField(o.Object, "status", "phase") != "Verified" {
			return o, nil
		}
	}
	return nil, nil
}

// A source instance can only be fenced for the immutable operation that owns it.
func requestGroupInfrastructureFence(ctx context.Context, c client.Client, req *unstructured.Unstructured) error {
	if req.GetAnnotations()[annotationEmergencyEventID] == "" {
		return nil
	}
	np := p.NewObject("NodeProvision")
	a := req.GetAnnotations()
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.GetNamespace(), Name: a[groupOldName]}, np); err != nil {
		return err
	}
	if string(np.GetUID()) != a[groupOldUID] || np.GetLabels()[p.LabelPolicyUID] != req.GetLabels()[p.LabelPolicyUID] {
		return fmt.Errorf("source instance fence ownership mismatch")
	}
	instance := stringField(np.Object, "status", "instanceId")
	sources, _, _ := unstructured.NestedSlice(req.Object, "spec", "groupRestore", "sourcePods")
	matched := false
	for _, raw := range sources {
		s := raw.(map[string]interface{})
		if stringField(s, "nodeProvisionRef", "name") == np.GetName() && stringField(s, "nodeProvisionRef", "uid") == stringField(np.Object, "status", "memberUID") && stringField(s, "nodeProvisionRef", "instanceID") == instance {
			matched = true
		}
	}
	if !matched || instance == "" {
		return fmt.Errorf("source instance fence identity mismatch")
	}
	desired := map[string]interface{}{"operationUID": stringField(req.Object, "spec", "groupRestore", "operationUID"), "instanceID": instance}
	if existing, found, _ := unstructured.NestedMap(np.Object, "spec", "fence"); found {
		if !reflect.DeepEqual(existing, desired) {
			return fmt.Errorf("source instance fence conflict")
		}
		return nil
	}
	before := np.DeepCopy()
	_ = unstructured.SetNestedMap(np.Object, desired, "spec", "fence")
	return c.Patch(ctx, np, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
