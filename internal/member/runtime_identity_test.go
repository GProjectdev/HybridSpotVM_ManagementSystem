package member

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestUnavailableRuntimeStillCapturesCurrentSourceUID(t *testing.T) {
	yes := true
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "trainer", Namespace: "demo", UID: "member-world", Labels: map[string]string{"training.dcnlab.com/workload-uid": "origin-world"}}, Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "trainer"}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "trainer-0", Namespace: "demo", UID: "current-uid", Labels: map[string]string{"app": "trainer"}, OwnerReferences: []metav1.OwnerReference{{UID: sts.UID, Controller: &yes}}}, Spec: corev1.PodSpec{NodeName: "lost-node"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts, pod).Build()
	r := &RuntimeReconciler{Client: c}
	status := map[string]interface{}{}
	if err := r.collect(context.Background(), "demo", "trainer", "origin-world", 1, 8298, status); err == nil {
		t.Fatal("unhealthy runtime accepted")
	}
	snapshots, ok := status["sourcePods"].([]interface{})
	if !ok || len(snapshots) != 1 || snapshots[0].(map[string]interface{})["uid"] != "current-uid" || status["sourceWorldUID"] != "origin-world" {
		t.Fatalf("missing current identity: %#v", status)
	}
	if status["pods"] != nil {
		t.Fatal("unhealthy identity treated as ready runtime")
	}
	pod.OwnerReferences[0].UID = "another-world"
	if got := sourcePodIdentities(sts, &corev1.PodList{Items: []corev1.Pod{*pod}}, 1); got != nil {
		t.Fatal("foreign owner adopted")
	}
}
