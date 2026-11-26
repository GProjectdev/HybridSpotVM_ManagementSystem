package framework

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GPUHeadroom is a fresh upper bound, not a substitute Kubernetes scheduler.
// It only excludes a member when even its total schedulable free GPUs cannot
// hold this whole world. Local scheduling still checks affinity, CPU and memory.
func initialGPUHeadroom(ctx context.Context, reader client.Reader, work *unstructured.Unstructured, world int64) (bool, map[string]interface{}, error) {
	raw, found, err := unstructured.NestedMap(work.Object, "spec", "template", "spec")
	if err != nil || !found {
		return false, nil, fmt.Errorf("Pod template required for GPU headroom")
	}
	var spec corev1.PodSpec
	if err = runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &spec); err != nil {
		return false, nil, err
	}
	per := gpuRequest(spec)
	if per < 1 {
		return false, nil, fmt.Errorf("positive GPU request required")
	}
	nodes := &corev1.NodeList{}
	if err = reader.List(ctx, nodes); err != nil {
		return false, nil, err
	}
	pods := &corev1.PodList{}
	if err = reader.List(ctx, pods); err != nil {
		return false, nil, err
	}
	free := map[string]int64{}
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready || n.Spec.Unschedulable || !n.DeletionTimestamp.IsZero() {
			continue
		}
		q := n.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
		free[n.Name] = q.Value()
	}
	// Own, never-admitted scheduled Pods can be reused. A foreign same-name Pod
	// cannot be added back: membership must bind to this exact source workload.
	member := object("apps/v1", "StatefulSet")
	ownUID := ""
	if err = reader.Get(ctx, client.ObjectKeyFromObject(work), member); err == nil && member.GetLabels()["training.dcnlab.com/workload-uid"] == string(work.GetUID()) {
		ownUID = string(member.GetUID())
	} else if err != nil && !apierrors.IsNotFound(err) {
		return false, nil, err
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		owner := metav1.GetControllerOf(&pod)
		if ownUID != "" && owner != nil && string(owner.UID) == ownUID && pod.Namespace == work.GetNamespace() && pod.DeletionTimestamp.IsZero() {
			continue
		}
		if _, ok := free[pod.Spec.NodeName]; ok {
			free[pod.Spec.NodeName] -= gpuRequest(pod.Spec)
		}
	}
	slots := int64(0)
	for _, n := range free {
		if n > 0 {
			slots += n / per
		}
	}
	return slots >= world, map[string]interface{}{"requiredRanks": world, "gpuPerRank": per, "availableRankUpperBound": slots, "method": "fresh-Ready-node-allocatable-minus-bound-nonterminal-Pod-GPU-requests"}, nil
}
func gpuRequest(s corev1.PodSpec) int64 {
	resource := corev1.ResourceName("nvidia.com/gpu")
	value := func(c corev1.Container) int64 {
		q, ok := c.Resources.Requests[resource]
		if !ok {
			q = c.Resources.Limits[resource]
		}
		return q.Value()
	}
	total := int64(0)
	for _, c := range s.Containers {
		total += value(c)
	}
	sidecars, peak := int64(0), int64(0)
	for _, c := range s.InitContainers {
		q := value(c)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars += q
			peak = max(peak, sidecars)
		} else {
			peak = max(peak, sidecars+q)
		}
	}
	total = max(total+sidecars, peak)
	q := s.Overhead[resource]
	return total + q.Value()
}
