package member

import (
	"context"
	"encoding/json"
	"io"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
	"time"
)

type completionTransport func(*http.Request) (*http.Response, error)

func (f completionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAllRankCompletionRequiresSuccessfulCurrentPodReceipts(t *testing.T) {
	for _, mode := range []string{"valid", "mixed", "missing", "wrong-uid", "failure", "future", "stale"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			zero := int32(0)
			yes := true
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: "ns", UID: "world", Labels: map[string]string{"training.dcnlab.com/workload-uid": "origin"}}, Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "train"}}}}
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = appsv1.AddToScheme(scheme)
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts)
			for i, name := range []string{"train-0", "train-1"} {
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name), CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)), Labels: map[string]string{"app": "train"}, OwnerReferences: []metav1.OwnerReference{{UID: "world", Controller: &yes}}}, Spec: corev1.PodSpec{NodeName: "node"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: []string{"10.0.0.1", "10.0.0.2"}[i]}}
				builder = builder.WithObjects(p)
			}
			h := &http.Client{Transport: completionTransport(func(req *http.Request) (*http.Response, error) {
				rank := int64(0)
				if req.URL.Hostname() == "10.0.0.2" {
					rank = 1
				}
				s := RuntimeObservation{GlobalStep: 42, Rank: rank, WorldSize: 2, State: "Completed", ObservedAt: now.Format(time.RFC3339Nano), Completion: &CompletionObservation{PodUID: []string{"train-0", "train-1"}[rank], NodeName: "node", ExitCode: &zero, FinishedAt: now.Add(-time.Second).Format(time.RFC3339Nano)}}
				if rank == 1 {
					switch mode {
					case "mixed":
						s.State = "Running"
						s.Completion = nil
					case "missing":
						s.Completion = nil
					case "wrong-uid":
						s.Completion.PodUID = "old"
					case "failure":
						n := int32(1)
						s.Completion.ExitCode = &n
					case "future":
						s.Completion.FinishedAt = now.Add(time.Minute).Format(time.RFC3339Nano)
					case "stale":
						s.ObservedAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
					}
				}
				b, _ := json.Marshal(s)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
			})}
			r := &RuntimeReconciler{Client: builder.Build(), HTTP: h}
			status := map[string]interface{}{}
			e := r.collect(context.Background(), "ns", "train", "origin", 2, 8298, status)
			if mode == "valid" {
				if e != nil || status["phase"] != "Completed" || status["completedRanks"] != int64(2) || status["readyRanks"] != int64(0) {
					t.Fatal(status, e)
				}
			} else if mode == "mixed" {
				if e != nil || status["phase"] != "Completing" {
					t.Fatal(status, e)
				}
			} else if e == nil {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
}
