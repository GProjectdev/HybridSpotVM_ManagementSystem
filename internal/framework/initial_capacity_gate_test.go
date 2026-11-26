package framework

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestGPUHeadroomUsesAllNamespacesAndRetainsTerminatingOccupancy(t *testing.T) {
	for _, scenario := range []string{"occupied", "free", "pending", "completed", "terminating", "unready", "insufficient-world"} {
		t.Run(scenario, func(t *testing.T) {
			_, w := initialObjects("Execute")
			_ = unstructured.SetNestedMap(w.Object, map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "train", "resources": map[string]interface{}{"requests": map[string]interface{}{"nvidia.com/gpu": "1"}}}}}, "spec", "template", "spec")
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "other-namespace"}, Spec: corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "train", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}}}
			world := int64(1)
			switch scenario {
			case "free":
				pod.Spec.Containers = nil
			case "pending":
				pod.Spec.NodeName = ""
			case "completed":
				pod.Status.Phase = corev1.PodSucceeded
			case "terminating":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				pod.Finalizers = []string{"keep"}
			case "unready":
				n.Status.Conditions[0].Status = corev1.ConditionUnknown
			case "insufficient-world":
				world = 2
				pod.Spec.Containers = nil
			}
			c := frameworkClient(n, pod)
			got, _, err := initialGPUHeadroom(context.Background(), c, w, world)
			if err != nil {
				t.Fatal(err)
			}
			want := scenario == "free" || scenario == "pending" || scenario == "completed"
			if got != want {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
}
