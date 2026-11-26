package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type SourcePod struct {
	Rank             int64        `json:"rank"`
	PodName          string       `json:"podName"`
	PodUID           string       `json:"podUID"`
	NodeName         string       `json:"nodeName"`
	NodeProvisionRef *ProviderRef `json:"nodeProvisionRef,omitempty"`
}
type ProviderRef struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	InstanceID string `json:"instanceID"`
}

// An immutable transition separates policy intent from the sequence of effects.
// Added capacity never authorizes old capacity retirement on its own.
type TransitionSpec struct {
	WorkloadTemplateSHA256 string       `json:"workloadTemplateSHA256"`
	RunRef                 ObjectRef    `json:"runRef"`
	Mode                   string       `json:"mode"`
	WorkloadUID            string       `json:"workloadUID"`
	ExecutionID            string       `json:"executionID"`
	WorldSize              int64        `json:"worldSize"`
	SourceCluster          string       `json:"sourceCluster"`
	TargetCluster          string       `json:"targetCluster"`
	CapacityRef            ObjectRef    `json:"capacityRef"`
	SourcePods             []SourcePod  `json:"sourcePods"`
	TargetAllocations      []string     `json:"targetAllocations"`
	AddedAllocations       []Allocation `json:"addedAllocations,omitempty"`
	RetireAllocations      []string     `json:"retireAllocations,omitempty"`
	EmergencyDeadline      string       `json:"emergencyDeadline,omitempty"`
	TriggerID              string       `json:"triggerID"`
}

func executionID(run *unstructured.Unstructured) string {
	permit, found, _ := unstructured.NestedMap(run.Object, "status", "executionPermit")
	if !found {
		return ""
	}
	// Node inventory can be filled later without changing the admitted world.
	delete(permit, "nodes")
	b, _ := json.Marshal(permit)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (s TransitionSpec) validate(run, work, t *unstructured.Unstructured, rs RunSpec) error {
	if s.WorkloadTemplateSHA256 != workloadTemplateSHA(work) {
		return fmt.Errorf("workload template changed during transition")
	}
	if !owned(t, run) || s.RunRef.Name != run.GetName() || s.RunRef.UID != string(run.GetUID()) || s.WorkloadUID != string(work.GetUID()) || s.WorldSize != rs.WorldSize || s.ExecutionID == "" || s.TriggerID == "" {
		return fmt.Errorf("transition owner/workload contract mismatch")
	}
	if s.Mode != "PlannedReplacement" && s.Mode != "Preemption" && s.Mode != "Failover" {
		return fmt.Errorf("unsupported transition mode")
	}
	if s.TargetCluster != rs.Cloud.Cluster || !rs.Cloud.Allowed || s.SourceCluster == "" || (s.SourceCluster != rs.OnPremiseCluster && s.SourceCluster != rs.Cloud.Cluster) || s.CapacityRef.UID == "" || s.CapacityRef.Name != initialCapacityName(run) {
		return fmt.Errorf("unapproved transition capacity or member")
	}
	if int64(len(s.SourcePods)) != s.WorldSize || int64(len(s.TargetAllocations)) != s.WorldSize {
		return fmt.Errorf("complete source and target world required")
	}
	seen := map[string]bool{}
	rank := map[int64]bool{}
	for _, src := range s.SourcePods {
		if src.Rank < 0 || src.Rank >= s.WorldSize || rank[src.Rank] || src.PodName != fmt.Sprintf("%s-%d", work.GetName(), src.Rank) || src.PodUID == "" || seen[src.PodUID] || src.NodeName == "" {
			return fmt.Errorf("invalid source world identity")
		}
		rank[src.Rank] = true
		seen[src.PodUID] = true
		if s.SourceCluster == rs.Cloud.Cluster && (src.NodeProvisionRef == nil || src.NodeProvisionRef.UID == "" || src.NodeProvisionRef.InstanceID == "") {
			return fmt.Errorf("source provider identity required")
		}
	}
	seen = map[string]bool{}
	for _, name := range s.TargetAllocations {
		if name == "" || seen[name] {
			return fmt.Errorf("unique target allocations required")
		}
		seen[name] = true
	}
	for _, name := range s.RetireAllocations {
		if seen[name] {
			return fmt.Errorf("target cannot also be retired")
		}
	}
	if s.Mode != "PlannedReplacement" {
		if _, e := time.Parse(time.RFC3339Nano, s.EmergencyDeadline); e != nil {
			return fmt.Errorf("bounded emergency deadline required")
		}
	}
	return nil
}
func transitionInput(run, work *unstructured.Unstructured, s TransitionSpec) p.PolicyInput {
	return p.PolicyInput{Namespace: run.GetNamespace(), PolicyName: run.GetName(), PolicyUID: run.GetUID(), SourceCluster: s.SourceCluster, TargetWorkers: s.WorldSize, RuntimeRefName: run.GetName() + "-runtime", WorkloadRef: p.WorkloadRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: work.GetName(), UID: work.GetUID()}, Checkpoint: p.CheckpointPolicy{Resume: true}}
}
func activeTransition(ctx context.Context, c client.Reader, run *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	items, e := listItems(ctx, c, "training.dcnlab.com/v1alpha1", "TrainingTransition", run.GetNamespace())
	if e != nil {
		return nil, e
	}
	var selected *unstructured.Unstructured
	for i := range items {
		t := &items[i]
		if !owned(t, run) {
			continue
		}
		phase := str(t, "status", "phase")
		if phase == "Completed" || phase == "Cancelled" {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("multiple active transitions")
		}
		selected = t
	}
	return selected, nil
}
func targetCapacity(cap *unstructured.Unstructured, s TransitionSpec, now time.Time) (*unstructured.Unstructured, error) {
	if string(cap.GetUID()) != s.CapacityRef.UID || integer(cap, "status", "observedGeneration") != cap.GetGeneration() || !cap.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("target capacity identity or observation is stale")
	}
	at, e := time.Parse(time.RFC3339Nano, str(cap, "status", "observedAt"))
	if e != nil || at.After(now) || now.Sub(at) > 30*time.Second {
		return nil, fmt.Errorf("target capacity observation expired")
	}
	var cs CapacitySpec
	if decodeSpec(cap, &cs) != nil {
		return nil, fmt.Errorf("invalid capacity")
	}
	deadline, e := time.Parse(time.RFC3339, cs.Deadline)
	if e != nil || !now.Before(deadline) {
		return nil, fmt.Errorf("capacity deadline expired")
	}
	target := []Allocation{}
	nodes := map[string]bool{}
	for _, name := range s.TargetAllocations {
		var allocation *Allocation
		for i := range cs.Allocations {
			if cs.Allocations[i].Name == name {
				allocation = &cs.Allocations[i]
			}
		}
		n := str(cap, "status", "allocations", name, "nodeName")
		if allocation == nil || allocation.State == "Retire" || allocation.State == "Fence" || str(cap, "status", "allocations", name, "phase") != "Ready" || n == "" || nodes[n] || str(cap, "status", "allocations", name, "nodeUID") == "" || str(cap, "status", "allocations", name, "memberUID") == "" {
			return nil, fmt.Errorf("waiting for exact target allocation %s", name)
		}
		nodes[n] = true
		target = append(target, *allocation)
	}
	cs.Allocations = target
	view := cap.DeepCopy()
	view.Object["spec"] = encode(cs)
	return view, nil
}

func workloadTemplateSHA(work *unstructured.Unstructured) string {
	template, _, _ := unstructured.NestedMap(work.Object, "spec", "template")
	b, _ := json.Marshal(template)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
