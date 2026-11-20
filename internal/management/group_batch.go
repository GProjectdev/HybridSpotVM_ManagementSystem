package management

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const groupBatchSizeAnnotation = "training.dcnlab.com/group-replacement-batch-size"
const groupBatchPairsAnnotation = "training.dcnlab.com/group-replacement-pairs"

// Opt-in bounded overlap; absent means the compatible single-worker path.
func groupBatchSize(policy *unstructured.Unstructured, world int64) (int, error) {
	raw := policy.GetAnnotations()[groupBatchSizeAnnotation]
	if raw == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 4 || int64(n) > world {
		return 0, fmt.Errorf("group replacement batch size must be between 1 and min(worldSize,4)")
	}
	return n, nil
}

type groupBatchTarget struct{ OldName, OldUID, Replacement, Market string }
type groupReplacementPair struct{ OldName, OldUID, NewName, NewUID string }

// Legacy annotations remain the primary pair for existing emergency handlers.
// Batch receipts are an immutable list of all provider identities, never names alone.
func groupReplacementPairs(req *unstructured.Unstructured) ([]groupReplacementPair, error) {
	a := req.GetAnnotations()
	primary := groupReplacementPair{a[groupOldName], a[groupOldUID], a[groupNewName], a[groupNewUID]}
	if primary == (groupReplacementPair{}) && a[groupBatchPairsAnnotation] == "" {
		return nil, nil
	}
	pairs := []groupReplacementPair{primary}
	if raw := a[groupBatchPairsAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &pairs); err != nil {
			return nil, err
		}
		if len(pairs) < 2 || len(pairs) > 4 || pairs[0] != primary || a[groupCleanupOwner] != "" || a[groupCleanupOwnerUID] != "" || a[annotationEmergencyEventID] != "" {
			return nil, fmt.Errorf("invalid policy batch replacement receipt")
		}
	}
	seenNames, seenUIDs := map[string]bool{}, map[string]bool{}
	for _, pair := range pairs {
		if pair.OldName == "" || pair.OldUID == "" || pair.NewName == "" || pair.NewUID == "" || pair.OldName == pair.NewName || pair.OldUID == pair.NewUID || seenNames[pair.OldName] || seenNames[pair.NewName] || seenUIDs[pair.OldUID] || seenUIDs[pair.NewUID] {
			return nil, fmt.Errorf("incomplete or duplicated group replacement identities")
		}
		seenNames[pair.OldName], seenNames[pair.NewName], seenUIDs[pair.OldUID], seenUIDs[pair.NewUID] = true, true, true, true
	}
	return pairs, nil
}

func validateGroupBatchIntent(policy *unstructured.Unstructured, input p.PolicyInput, intent groupOperation) error {
	limit, err := groupBatchSize(policy, input.TargetWorkers)
	if err != nil {
		return err
	}
	if input.Suspended || input.Capacity.Mode == p.CapacityModeExisting || !boolField(policy.Object, "spec", "replacement", "enabled") || policy.GetAnnotations()["training.dcnlab.com/planned-partial"] != "disabled" || intent.Event != "" || len(intent.Batch) < 2 || len(intent.Batch) > limit || intent.PolicyUID != string(input.PolicyUID) || intent.PolicyGeneration != policy.GetGeneration() {
		return fmt.Errorf("policy batch is disabled or has stale/bounded intent")
	}
	first := intent.Batch[0]
	if first.OldName != intent.OldName || first.OldUID != intent.OldUID || first.Replacement != intent.Replacement || first.Market != intent.Market || intent.Operation != replacementOperationName(first.OldName, first.OldUID) {
		return fmt.Errorf("policy batch primary identity mismatch")
	}
	seenNames, seenUIDs := map[string]bool{}, map[string]bool{}
	for _, t := range intent.Batch {
		if t.OldName == "" || t.OldUID == "" || t.Replacement != replacementNodeProvisionName(t.OldName, t.OldUID) || !validReplacementMarket(t.Market) || seenNames[t.OldName] || seenNames[t.Replacement] || seenUIDs[t.OldUID] {
			return fmt.Errorf("invalid policy batch target identity")
		}
		seenNames[t.OldName], seenNames[t.Replacement], seenUIDs[t.OldUID] = true, true, true
	}
	return nil
}

// Prepare all selected VMs before issuing ONE existing full-world RestoreRequest.
// No source Pod or VM is deleted here; normal fenced/Verified recovery owns cleanup.
func (r *PolicyReconciler) ensureGroupBatchReplacement(ctx context.Context, policy *unstructured.Unstructured, input p.PolicyInput, intent groupOperation) (bool, error) {
	if err := validateGroupBatchIntent(policy, input, intent); err != nil {
		return false, err
	}
	name := intent.Operation + "-group-restore"
	req := newRestoreRequest()
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: name}, req); err == nil {
		pairs, err := groupReplacementPairs(req)
		if err != nil {
			return false, err
		}
		if req.GetLabels()[p.LabelPolicyUID] != intent.PolicyUID || len(pairs) != len(intent.Batch) || stringField(req.Object, "spec", "groupRestore", "sourceWorldUID") != string(input.WorkloadRef.UID) {
			return false, fmt.Errorf("batch restore request collision")
		}
		for i, t := range intent.Batch {
			if pairs[i].OldName != t.OldName || pairs[i].OldUID != t.OldUID || pairs[i].NewName != t.Replacement {
				return false, fmt.Errorf("batch restore provider collision")
			}
		}
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
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
	sourceNodes := map[string]bool{}
	for _, raw := range source {
		sourceNodes[stringField(raw.(map[string]interface{}), "nodeName")] = true
	}
	olds := make([]*unstructured.Unstructured, len(intent.Batch))
	usedNodes := map[string]bool{}
	// Validate ALL sources before allocating ANY new provider.
	for i, t := range intent.Batch {
		old := p.NewObject("NodeProvision")
		if err := r.reader().Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: t.OldName}, old); err != nil {
			return false, err
		}
		node := stringField(old.Object, "status", "nodeName")
		if string(old.GetUID()) != t.OldUID || old.GetLabels()[p.LabelPolicyUID] != intent.PolicyUID || !old.GetDeletionTimestamp().IsZero() || stringField(old.Object, "status", "phase") != "Ready" || !sourceNodes[node] || usedNodes[node] || !validReplacementMarket(stringField(old.Object, "spec", "marketType")) || stringField(old.Object, "spec", "marketType") == t.Market {
			return false, fmt.Errorf("batch source identity/market/readiness mismatch")
		}
		usedNodes[node] = true
		olds[i] = old
	}
	if ready, err := r.quiesceGroup(ctx, policy, intent.OldUID); err != nil || !ready {
		return false, err
	}
	if err := r.recordGroupIntent(ctx, policy, intent); err != nil {
		return false, err
	}
	if r.GroupStage == groupStageIntent {
		return false, nil
	}
	nodes := map[string]string{}
	for node := range sourceNodes {
		nodes[node] = node
	}
	pairs := make([]groupReplacementPair, 0, len(olds))
	waiting := ""
	targetNodes := map[string]bool{}
	for i, t := range intent.Batch {
		target := p.NewObject("NodeProvision")
		key := client.ObjectKey{Namespace: input.Namespace, Name: t.Replacement}
		if err := r.reader().Get(ctx, key, target); apierrors.IsNotFound(err) {
			if r.GroupStage == groupStageRequest {
				waiting = t.Replacement
				continue
			}
			target.SetNamespace(input.Namespace)
			target.SetName(t.Replacement)
			target.SetLabels(map[string]string{p.LabelPolicyUID: intent.PolicyUID, p.LabelPolicy: input.PolicyName, p.LabelManagedBy: "hybridspotvm-system", p.LabelRole: groupReplacementRole})
			target.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": intent.Operation, groupOldUID: t.OldUID})
			spec, ok, _ := unstructured.NestedMap(olds[i].Object, "spec")
			if !ok {
				return false, fmt.Errorf("batch source spec missing")
			}
			delete(spec, "fence")
			unstructured.RemoveNestedField(spec, "awsConfig", "keyPairName")
			spec["marketType"] = t.Market
			spec["hostname"] = t.Replacement
			target.Object["spec"] = spec
			if err := r.Create(ctx, target); err != nil {
				return false, err
			}
		} else if err != nil {
			return false, err
		}
		if target.GetLabels()[p.LabelPolicyUID] != intent.PolicyUID || target.GetAnnotations()[groupOldUID] != t.OldUID || target.GetAnnotations()["training.dcnlab.com/recovery-operation"] != intent.Operation || stringField(target.Object, "spec", "marketType") != t.Market || !target.GetDeletionTimestamp().IsZero() {
			return false, fmt.Errorf("batch replacement identity mismatch")
		}
		if r.GroupStage != groupStageRequest {
			if err := r.createIfMissing(ctx, p.NewPropagationPolicyFor(input, target, input.Capacity.AWSCluster)); err != nil {
				return false, err
			}
		}
		node := stringField(target.Object, "status", "nodeName")
		if target.GetUID() == "" || stringField(target.Object, "status", "phase") != "Ready" || node == "" {
			waiting = t.Replacement
			continue
		}
		if sourceNodes[node] || targetNodes[node] {
			return false, fmt.Errorf("batch target nodes must be distinct new workers")
		}
		targetNodes[node] = true
		nodes[stringField(olds[i].Object, "status", "nodeName")] = node
		pairs = append(pairs, groupReplacementPair{t.OldName, t.OldUID, t.Replacement, string(target.GetUID())})
	}
	if r.GroupStage == groupStageCapacity {
		return false, nil
	}
	if waiting != "" {
		return false, fmt.Errorf("waiting for complete batch replacement capacity: %s", waiting)
	}
	// Never replay an old periodic round after a planned capacity change.
	round, err = r.ensurePlannedGroupCheckpoint(ctx, policy, input, intent.Operation, source, pairs)
	if err != nil || round == nil {
		return false, err
	}
	req, err = r.newGroupRequest(ctx, input, name, intent.OldUID, input.SourceCluster, round, source, pvc, root, nodes)
	if err != nil {
		return false, err
	}
	raw, err := json.Marshal(pairs)
	if err != nil {
		return false, err
	}
	a := req.GetAnnotations()
	a[groupBatchPairsAnnotation] = string(raw)
	a[groupOldName], a[groupOldUID], a[groupNewName], a[groupNewUID] = pairs[0].OldName, pairs[0].OldUID, pairs[0].NewName, pairs[0].NewUID
	req.SetAnnotations(a)
	// The recorded intent must still be identical immediately before request creation.
	current := p.NewObject("NodeRequest")
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
		return false, err
	}
	var pinned groupOperation
	if err := json.Unmarshal([]byte(current.GetAnnotations()[groupOperationAnnotation]), &pinned); err != nil {
		return false, err
	}
	if current.GetUID() != policy.GetUID() || current.GetGeneration() != policy.GetGeneration() || !current.GetDeletionTimestamp().IsZero() || p.ReadPolicyInput(current).Suspended || !reflect.DeepEqual(intent, pinned) {
		return false, fmt.Errorf("batch policy intent changed before restore")
	}
	if err := r.Create(ctx, req); err != nil {
		return false, err
	}
	return true, nil
}
