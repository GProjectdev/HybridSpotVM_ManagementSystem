package management

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type existingCapacityReadFailure struct {
	client.Reader
	failGet, failList bool
}

func (r existingCapacityReadFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r.failGet {
		return fmt.Errorf("member unavailable")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
func (r existingCapacityReadFailure) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if r.failList {
		return fmt.Errorf("pod list forbidden")
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestExistingCapacityNodes(t *testing.T) {
	for _, tc := range []struct {
		name              string
		mutate            func(*corev1.Node, *corev1.Pod, *p.PolicyInput)
		valid             bool
		gpu               int64
		failGet, failList bool
	}{
		{name: "ready-idle", valid: true, gpu: 1},
		{name: "unknown-mode", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) { in.Capacity.Mode = "Unknown" }},
		{name: "provisioned-without-nodes", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			in.Capacity.Mode = p.CapacityModeProvisioned
			in.Capacity.ExistingNodes = nil
		}},
		{name: "wrong-uid", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) { n.UID = "recreated" }},
		{name: "not-ready", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			n.Status.Conditions[0].Status = corev1.ConditionFalse
		}},
		{name: "unknown-ready", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			n.Status.Conditions[0].Status = corev1.ConditionUnknown
		}},
		{name: "missing-ready", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) { n.Status.Conditions = nil }},
		{name: "cordoned", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) { n.Spec.Unschedulable = true }},
		{name: "deleting", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			now := metav1.Now()
			n.DeletionTimestamp = &now
			n.Finalizers = []string{"external-owner"}
		}},
		{name: "no-gpu", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) { n.Status.Allocatable = nil }},
		{name: "insufficient-gpu", gpu: 2},
		{name: "invalid-demand", gpu: 0},
		{name: "no-schedule-gpu-taint", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			n.Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}}
		}},
		{name: "no-schedule-even-if-job-tolerates", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			n.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule}}
		}},
		{name: "prefer-no-schedule-is-advisory", valid: true, gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectPreferNoSchedule}}
		}},
		{name: "evicting-node", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			n.Spec.Taints = []corev1.Taint{{Key: "failed", Effect: corev1.TaintEffectNoExecute}}
		}},
		{name: "occupied-other-namespace", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{existingCapacityGPU: resource.MustParse("1")}
		}},
		{name: "occupied-request", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{existingCapacityGPU: resource.MustParse("1")}
		}},
		{name: "occupied-init", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			pod.Spec.InitContainers = []corev1.Container{{Name: "init", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{existingCapacityGPU: resource.MustParse("1")}}}}
		}},
		{name: "terminating-gpu-pod", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			pod.Finalizers = []string{"external-owner"}
			pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{existingCapacityGPU: resource.MustParse("1")}
		}},
		{name: "completed-gpu-pod", valid: true, gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			pod.Status.Phase = corev1.PodSucceeded
			pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{existingCapacityGPU: resource.MustParse("1")}
		}},
		{name: "no-opt-in", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) { in.Capacity.Mode = "Provisioned" }},
		{name: "wrong-cluster", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) { in.Capacity.AWSCluster = "other" }},
		{name: "same-cluster", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) { in.SourceCluster = "aws" }},
		{name: "missing-node", gpu: 1, mutate: func(n *corev1.Node, pod *corev1.Pod, in *p.PolicyInput) {
			in.Capacity.ExistingNodes[0].NodeName = "missing"
		}},
		{name: "get-failure", gpu: 1, failGet: true},
		{name: "list-failure", gpu: 1, failList: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := p.PolicyInput{Namespace: "training", SourceCluster: "onpre1", TargetWorkers: 1, WorkloadRef: p.WorkloadRef{Name: "trainer", UID: "workload-uid"}, Capacity: p.CapacityDefaults{Mode: p.CapacityModeExisting, AWSCluster: "aws", ExistingNodes: []p.ExistingCapacityNode{{Rank: 0, NodeName: "aws-gpu-demo-worker-01", NodeUID: "node-uid"}}}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aws-gpu-demo-worker-01", UID: "node-uid", Labels: map[string]string{"external-owner": "demo"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, Allocatable: corev1.ResourceList{existingCapacityGPU: resource.MustParse("1")}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other-app", Namespace: "other-tenant"}, Spec: corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Name: "app"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			if tc.mutate != nil {
				tc.mutate(node, pod, &input)
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod).Build()
			before := &corev1.Node{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(node), before); err != nil {
				t.Fatal(err)
			}
			reader := existingCapacityReadFailure{Reader: c, failGet: tc.failGet, failList: tc.failList}
			names, err := ExistingCapacityNodes(context.Background(), reader, input, "aws", tc.gpu)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v nodes=%v err=%v", tc.valid, names, err)
			}
			if tc.valid && names["trainer-0"] != node.Name {
				t.Fatalf("mapping=%v", names)
			}
			if !tc.valid && names != nil {
				t.Fatal("partial mapping escaped failed validation")
			}
			after := &corev1.Node{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(node), after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("external node was mutated")
			}
		})
	}
}
