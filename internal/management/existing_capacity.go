package management

import (
	"context"
	"fmt"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const existingCapacityGPU corev1.ResourceName = "nvidia.com/gpu"

// ExistingCapacityNodes validates externally owned member Nodes without adopting
// or modifying them. Supply a live reader for target and block on any error.
// Nodes must already be uncordoned and free of all NoSchedule/NoExecute taints;
// preparation Job tolerations are deliberately not assumed by this preflight.
// This preflight does not reserve devices against concurrent scheduling.
func ExistingCapacityNodes(ctx context.Context, reader client.Reader, input p.PolicyInput, target string, gpuPerRank int64) (map[string]string, error) {
	if err := p.ValidateCapacity(input); err != nil {
		return nil, err
	}
	if input.Capacity.Mode != p.CapacityModeExisting {
		return nil, fmt.Errorf("Existing capacity opt-in required")
	}
	if target == "" || target != input.Capacity.AWSCluster || target == input.SourceCluster {
		return nil, fmt.Errorf("Existing capacity requires cross-cluster placement to the configured AWS cluster")
	}
	if reader == nil {
		return nil, fmt.Errorf("target member reader required")
	}
	if input.WorkloadRef.Name == "" || input.WorkloadRef.UID == "" {
		return nil, fmt.Errorf("UID-bound workload required")
	}
	if gpuPerRank <= 0 {
		return nil, fmt.Errorf("positive GPU requirement per rank required")
	}
	minimum := *resource.NewQuantity(gpuPerRank, resource.DecimalSI)
	names := map[string]string{}
	for _, entry := range input.Capacity.ExistingNodes {
		node := &corev1.Node{}
		if err := reader.Get(ctx, client.ObjectKey{Name: entry.NodeName}, node); err != nil {
			return nil, fmt.Errorf("read existing Node %s: %w", entry.NodeName, err)
		}
		if node.Name != entry.NodeName || node.UID != entry.NodeUID || !node.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("existing Node %s identity changed or is deleting", entry.NodeName)
		}
		if node.Spec.Unschedulable {
			return nil, fmt.Errorf("existing Node %s is unschedulable", entry.NodeName)
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				ready = condition.Status == corev1.ConditionTrue
				break
			}
		}
		if !ready {
			return nil, fmt.Errorf("existing Node %s is not Ready", entry.NodeName)
		}
		for _, taint := range node.Spec.Taints {
			if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
				return nil, fmt.Errorf("existing Node %s has a %s taint", entry.NodeName, taint.Effect)
			}
		}
		gpu := node.Status.Allocatable[existingCapacityGPU]
		if gpu.Cmp(minimum) < 0 {
			return nil, fmt.Errorf("existing Node %s has insufficient allocatable GPU", entry.NodeName)
		}
		names[fmt.Sprintf("%s-%d", input.WorkloadRef.Name, entry.Rank)] = entry.NodeName
	}
	// Inspect every namespace: a namespace-local list can hide occupied devices.
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods); err != nil {
		return nil, fmt.Errorf("list target member Pods: %w", err)
	}
	selected := map[string]bool{}
	for _, name := range names {
		selected[name] = true
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !selected[pod.Spec.NodeName] || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if existingCapacityPodUsesGPU(pod) {
			return nil, fmt.Errorf("existing Node %s GPU is occupied by Pod %s/%s", pod.Spec.NodeName, pod.Namespace, pod.Name)
		}
	}
	return names, nil
}

func existingCapacityPodUsesGPU(pod *corev1.Pod) bool {
	uses := func(r corev1.ResourceRequirements) bool {
		request, limit := r.Requests[existingCapacityGPU], r.Limits[existingCapacityGPU]
		return request.Sign() > 0 || limit.Sign() > 0
	}
	for _, container := range pod.Spec.Containers {
		if uses(container.Resources) {
			return true
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if uses(container.Resources) {
			return true
		}
	}
	for _, container := range pod.Spec.EphemeralContainers {
		if uses(container.Resources) {
			return true
		}
	}
	overhead := pod.Spec.Overhead[existingCapacityGPU]
	return overhead.Sign() > 0
}
