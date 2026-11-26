package framework

import (
	"context"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestWorkloadObservationRejectsStaleSplitAndPartialBinding(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []string{"oldUID", "split", "partial", "noBinding", "imageError", "gpuShortage"} {
		t.Run(tc, func(t *testing.T) {
			run := named("TrainingRun", "ns", "a")
			run.SetUID("run")
			work := object("apps/v1", "StatefulSet")
			work.SetName("a")
			work.SetNamespace("ns")
			work.SetUID("source")
			b := object("work.karmada.io/v1alpha2", "ResourceBinding")
			b.SetName("a")
			b.SetNamespace("ns")
			ref := map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "namespace": "ns", "name": "a", "uid": "source"}
			clusters := []interface{}{}
			if tc == "oldUID" {
				ref["uid"] = "old"
			}
			if tc == "split" {
				clusters = []interface{}{map[string]interface{}{"name": "onpre1", "replicas": int64(1)}, map[string]interface{}{"name": "aws1", "replicas": int64(1)}}
			}
			if tc == "partial" {
				clusters = []interface{}{map[string]interface{}{"name": "onpre1", "replicas": int64(1)}}
			}
			b.Object["spec"] = map[string]interface{}{"resource": ref, "clusters": clusters}
			msg := "ImagePullBackOff"
			if tc == "gpuShortage" {
				msg = "Insufficient nvidia.com/gpu"
			}
			b.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Scheduled", "status": "False", "message": msg}}}
			builder := fake.NewClientBuilder().WithLists(objects("work.karmada.io/v1alpha2", "ResourceBinding"))
			if tc != "noBinding" {
				builder = builder.WithObjects(b)
			}
			c := builder.Build()
			got, err := ObserveWorkload(ctx, c, func(string) (client.Reader, error) {
				t.Fatal("member read on invalid/unassigned binding")
				return nil, nil
			}, run, work, RunSpec{WorldSize: 2, OnPremiseCluster: "onpre1", Cloud: CloudSpec{Cluster: "aws1"}}, time.Now())
			bad := tc == "oldUID" || tc == "split" || tc == "partial"
			if (err != nil) != bad {
				t.Fatalf("got %v", err)
			}
			if got.Running || got.GPUShortage != (tc == "gpuShortage") {
				t.Fatalf("invalid observation %+v", got)
			}
		})
	}
}

func TestWorkloadObservationUsesCurrentMemberPodUIDs(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	run := named("TrainingRun", "ns", "a")
	run.SetUID("run")
	work := object("apps/v1", "StatefulSet")
	work.SetName("a")
	work.SetNamespace("ns")
	work.SetUID("origin")
	b := object("work.karmada.io/v1alpha2", "ResourceBinding")
	b.SetName("a")
	b.SetNamespace("ns")
	b.Object["spec"] = map[string]interface{}{"resource": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "namespace": "ns", "name": "a", "uid": "origin"}, "clusters": []interface{}{map[string]interface{}{"name": "onpre1", "replicas": int64(1)}}}
	rt := named("TrainingRuntime", "ns", "a-runtime")
	rt.SetGeneration(1)
	setOwner(rt, run)
	rt.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "origin"}, "sourceCluster": "onpre1", "expectedWorldSize": int64(1)}
	status := map[string]interface{}{"phase": "Running", "workloadUID": "origin", "memberWorkloadUID": "member", "observedGeneration": int64(1), "worldSize": int64(1), "readyRanks": int64(1), "globalStep": int64(10), "observedAt": stamp(now), "pods": []interface{}{map[string]interface{}{"name": "a-0", "uid": "pod", "rank": int64(0), "globalStep": int64(10), "observedAt": stamp(now)}}}
	rt.Object["status"] = status
	s := runtime.NewScheme()
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", UID: "member", Labels: map[string]string{"training.dcnlab.com/workload-uid": "origin"}}, Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}}}}
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a-0", Namespace: "ns", UID: "pod", Labels: map[string]string{"app": "a"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "a", UID: "member", Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: "gpu"}}
	for _, currentUID := range []types.UID{"pod", "replacement-pod"} {
		pod.UID = currentUID
		member := fake.NewClientBuilder().WithScheme(s).WithObjects(sts, pod).Build()
		control := fake.NewClientBuilder().WithObjects(b, rt).Build()
		got, err := ObserveWorkload(ctx, control, func(string) (client.Reader, error) { return member, nil }, run, work, RunSpec{WorldSize: 1, OnPremiseCluster: "onpre1"}, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Running != (currentUID == "pod") {
			t.Fatalf("stale rank accepted or current rejected: %+v", got)
		}
	}
}

func TestCandidateRuntimeMayMoveOnlyBeforeExecutionCommitment(t *testing.T) {
	ctx := context.Background()
	run, work := initialObjects("Execute")
	b := object("work.karmada.io/v1alpha2", "ResourceBinding")
	b.SetName("a")
	b.SetNamespace("ns")
	b.Object["spec"] = map[string]interface{}{"resource": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "namespace": "ns", "name": "a", "uid": string(work.GetUID())}, "clusters": []interface{}{map[string]interface{}{"name": "aws1", "replicas": int64(1)}}}
	rt := named("TrainingRuntime", "ns", "a-runtime")
	setOwner(rt, run)
	rt.Object["spec"] = map[string]interface{}{"sourceCluster": "onpre1"}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", UID: "member", Labels: map[string]string{"training.dcnlab.com/workload-uid": string(work.GetUID())}}, Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}}}}
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	member := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	for _, state := range []string{"waiting", "committed", "permit", "ungated"} {
		t.Run(state, func(t *testing.T) {
			obj := run.DeepCopy()
			s := RunSpec{WorldSize: 1, OnPremiseCluster: "onpre1", ExecutionGate: true, Cloud: CloudSpec{Cluster: "aws1"}}
			switch state {
			case "committed":
				obj.Object["status"] = map[string]interface{}{"committedCluster": "onpre1"}
			case "permit":
				obj.Object["status"] = map[string]interface{}{"executionPermit": map[string]interface{}{"cluster": "onpre1"}}
			case "ungated":
				s.ExecutionGate = false
			}
			control := frameworkClient(obj, work, b, rt)
			o, err := ObserveWorkload(ctx, control, func(string) (client.Reader, error) { return member, nil }, obj, work, s, time.Now())
			if (err == nil) != (state == "waiting") || o.Running {
				t.Fatal(state, o, err)
			}
		})
	}
}
