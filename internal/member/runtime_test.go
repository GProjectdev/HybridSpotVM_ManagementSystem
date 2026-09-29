package member

import (
	"context"
	"encoding/json"
	"fmt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRuntimeRejectsDuplicateAndStaleRanks(t *testing.T) {
	observation := RuntimeObservation{GlobalStep: 42, CheckpointID: "round-1", Rank: 0, WorldSize: 2, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), State: "Running"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(observation) }))
	defer server.Close()
	host, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.ParseInt(portStr, 10, 64)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "trainer", Namespace: "default", UID: "member-sts", Labels: map[string]string{"training.dcnlab.com/workload-uid": "mgmt-sts"}}, Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "trainer"}}}}
	owner := true
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "trainer-0", Namespace: "default", UID: "pod0", Labels: map[string]string{"app": "trainer"}, OwnerReferences: []metav1.OwnerReference{{UID: sts.UID, Controller: &owner}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host}}
	p2 := p.DeepCopy()
	p2.Name = "trainer-1"
	p2.UID = "pod1"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts, p, p2).Build()
	r := &RuntimeReconciler{Client: c, HTTP: server.Client()}
	if err := r.collect(context.Background(), "default", "trainer", "mgmt-sts", 2, port, map[string]interface{}{}); err == nil {
		t.Fatal("duplicate rank accepted")
	}
	observation.WorldSize = 1
	observation.ObservedAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := r.collect(context.Background(), "default", "trainer", "mgmt-sts", 1, port, map[string]interface{}{}); err == nil {
		t.Fatal("stale rank accepted")
	}
	if err := r.collect(context.Background(), "default", "trainer", "different", 2, port, map[string]interface{}{}); err == nil {
		t.Fatal("origin UID mismatch accepted")
	}
}

func TestSurvivorReceiptPreservesLoadedCheckpoint(t *testing.T) {
	for _, mode := range []string{"valid", "uid", "node", "rank", "time", "checkpoint", "missing", "http"} {
		t.Run(mode, func(t *testing.T) {
			var calls int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "http" {
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, `{"error":"runtime survivor pause-lock proof is missing"}`)
					return
				}
				rank := calls
				calls++
				sample := RuntimeObservation{GlobalStep: 42, CheckpointID: "partial", Rank: rank, WorldSize: 2, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), State: "Running"}
				if rank == 0 {
					sample.CheckpointID = "previous-periodic"
					sample.SurvivorResume = map[string]interface{}{"checkpointID": "partial", "generation": 12, "podUID": "pod0", "nodeName": "node-0", "rank": 0, "resumedAt": time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)}
					switch mode {
					case "uid":
						sample.SurvivorResume["podUID"] = "other"
					case "node":
						sample.SurvivorResume["nodeName"] = "other"
					case "rank":
						sample.SurvivorResume["rank"] = 1
					case "time":
						sample.SurvivorResume["resumedAt"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
					case "checkpoint":
						sample.SurvivorResume["checkpointID"] = "other"
					case "missing":
						sample.SurvivorResume = nil
					}
				}
				_ = json.NewEncoder(w).Encode(sample)
			}))
			defer server.Close()
			host, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
			port, _ := strconv.ParseInt(portStr, 10, 64)
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = appsv1.AddToScheme(scheme)
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "trainer", Namespace: "default", UID: "member-sts", Labels: map[string]string{"training.dcnlab.com/workload-uid": "origin"}}, Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "trainer"}}}}
			owner := true
			objects := []runtime.Object{sts}
			for i := 0; i < 2; i++ {
				objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("trainer-%d", i), Namespace: "default", UID: types.UID(fmt.Sprintf("pod%d", i)), Labels: map[string]string{"app": "trainer"}, OwnerReferences: []metav1.OwnerReference{{UID: sts.UID, Controller: &owner}}}, Spec: corev1.PodSpec{NodeName: fmt.Sprintf("node-%d", i)}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host}})
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
			r := &RuntimeReconciler{Client: c, HTTP: server.Client()}
			status := map[string]interface{}{}
			err := r.collect(context.Background(), "default", "trainer", "origin", 2, port, status)
			if mode != "valid" {
				if err == nil {
					t.Fatal("invalid receipt accepted")
				}
				if mode == "http" && !strings.Contains(err.Error(), "503: runtime survivor pause-lock proof is missing") {
					t.Fatalf("diagnostic lost: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			pods := status["pods"].([]interface{})
			if status["checkpointID"] != "partial" || pods[0].(map[string]interface{})["checkpointID"] != "previous-periodic" {
				t.Fatalf("loaded checkpoint rewritten: %#v", status)
			}
		})
	}
}

func TestRuntimeCollectIncludesPodNodeName(t *testing.T) {
	var rank int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observation := RuntimeObservation{GlobalStep: 42, CheckpointID: "round-1", Rank: rank, WorldSize: 2, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), State: "Running"}
		rank++
		_ = json.NewEncoder(w).Encode(observation)
	}))
	defer server.Close()
	host, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.ParseInt(portStr, 10, 64)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "trainer", Namespace: "default", UID: "member-sts", Labels: map[string]string{"training.dcnlab.com/workload-uid": "mgmt-sts"}}, Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "trainer"}}}}
	owner := true
	objects := []runtime.Object{sts}
	for i := 0; i < 2; i++ {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("trainer-%d", i), Namespace: "default", UID: types.UID(fmt.Sprintf("pod%d", i)), Labels: map[string]string{"app": "trainer"}, OwnerReferences: []metav1.OwnerReference{{UID: sts.UID, Controller: &owner}}},
			Spec:       corev1.PodSpec{NodeName: fmt.Sprintf("node-%d", i)},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host},
		})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	r := &RuntimeReconciler{Client: c, HTTP: server.Client()}
	status := map[string]interface{}{}
	if err := r.collect(context.Background(), "default", "trainer", "mgmt-sts", 2, port, status); err != nil {
		t.Fatalf("collect runtime: %v", err)
	}
	pods := status["pods"].([]interface{})
	seen := map[string]bool{}
	for _, raw := range pods {
		pod := raw.(map[string]interface{})
		name := pod["name"].(string)
		node := pod["nodeName"].(string)
		seen[name+"@"+node] = true
	}
	if !seen["trainer-0@node-0"] || !seen["trainer-1@node-1"] {
		t.Fatalf("runtime pods missing nodeName evidence: %#v", pods)
	}
}
