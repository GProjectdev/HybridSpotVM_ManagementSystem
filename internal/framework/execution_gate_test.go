package framework

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestExecutionPermitCannotAuthorizeDifferentIncarnation(t *testing.T) {
	run, work := initialObjects("Execute")
	status := map[string]interface{}{"workloadUID": "source", "committedCluster": "onpre1"}
	o := WorkloadObservation{AllPodsScheduled: true, MemberWorkloadUID: "member", SelectedCluster: "onpre1", ScheduledPods: map[string]string{"a-0": "pod-uid"}}
	if err := initialPermit(status, work, o, "time"); err != nil {
		t.Fatal(err)
	}
	run.Object["status"] = status
	if !ExecutionPermitted(run, "run", "source", "a-0", "pod-uid") {
		t.Fatal("current permit rejected")
	}
	for _, v := range [][4]string{{"old-run", "source", "a-0", "pod-uid"}, {"run", "old-workload", "a-0", "pod-uid"}, {"run", "source", "a-1", "pod-uid"}, {"run", "source", "a-0", "replacement-pod"}} {
		if ExecutionPermitted(run, v[0], v[1], v[2], v[3]) {
			t.Fatalf("foreign permit accepted %v", v)
		}
	}
	o.ScheduledPods["a-0"] = "replacement-pod"
	if initialPermit(status, work, o, "later") == nil {
		t.Fatal("Pod replacement silently received an initial permit")
	}
	_ = unstructured.SetNestedField(run.Object, "aws1", "status", "committedCluster")
	if ExecutionPermitted(run, "run", "source", "a-0", "pod-uid") {
		t.Fatal("previous cluster permit accepted")
	}
}
func TestExecutionGateComparisonRetainsDefaultsButChecksFalse(t *testing.T) {
	expected := map[string]interface{}{"securityContext": map[string]interface{}{"allowPrivilegeEscalation": false}, "env": []interface{}{map[string]interface{}{"valueFrom": map[string]interface{}{"fieldRef": map[string]interface{}{"fieldPath": "metadata.uid"}}}}}
	got := map[string]interface{}{"securityContext": map[string]interface{}{"allowPrivilegeEscalation": false, "runAsUser": int64(65532)}, "env": []interface{}{map[string]interface{}{"valueFrom": map[string]interface{}{"fieldRef": map[string]interface{}{"apiVersion": "v1", "fieldPath": "metadata.uid"}}}}}
	if !containsSpec(got, expected) {
		t.Fatal("harmless API defaults rejected")
	}
	got["securityContext"].(map[string]interface{})["allowPrivilegeEscalation"] = true
	if containsSpec(got, expected) {
		t.Fatal("explicit false constraint ignored")
	}
}

func TestGateIsInjectedBeforePropagationAndCannotRetrofit(t *testing.T) {
	ctx := context.Background()
	run, work := initialObjects("Execute")
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "read-only-source", Namespace: "ns"}, Data: map[string][]byte{"kubeconfig": []byte("readonly test fixture")}}
	c := frameworkClient(run, work, source)
	r := &RunReconciler{Client: c, GateImage: "example.invalid/gate@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GateCredentialSecret: source.Name}
	spec := RunSpec{ExecutionGate: true, OnPremiseCluster: "onpre1"}
	if err := r.prepareExecutionGate(ctx, run, work, spec); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key("ns", "a"), work); err != nil {
		t.Fatal(err)
	}
	init, _, _ := unstructured.NestedSlice(work.Object, "spec", "template", "spec", "initContainers")
	if len(init) != 1 {
		t.Fatal("gate not injected")
	}
	if err := r.prepareExecutionGate(ctx, run, work, spec); err != nil {
		t.Fatal("idempotent gate rejected", err)
	}
	b := object("work.karmada.io/v1alpha2", "ResourceBinding")
	b.SetName("a")
	b.SetNamespace("ns")
	b.Object["spec"] = map[string]interface{}{"resource": map[string]interface{}{"uid": "source"}}
	if err := c.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(work.Object, "spec", "template", "spec", "initContainers")
	if err := r.prepareExecutionGate(ctx, run, work, spec); err == nil {
		t.Fatal("gate retrofitted after propagation")
	}
}
