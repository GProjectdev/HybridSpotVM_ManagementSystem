package framework

import (
	"context"
	"encoding/json"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func TestFailoverRequiresOptInAndConfirmedApplicationFailure(t *testing.T) {
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		enabled  bool
		restarts int32
		exit     int32
		reason   string
		want     bool
	}{
		{"not-opted-in", false, 5, 1, "CrashLoopBackOff", false},
		{"single-restart", true, 1, 1, "CrashLoopBackOff", false},
		{"checkpoint-exit", true, 5, 99, "CrashLoopBackOff", false},
		{"image-pull", true, 5, 1, "ImagePullBackOff", false},
		{"confirmed-crash", true, 3, 1, "CrashLoopBackOff", true},
		{"terminated-crash", true, 3, 1, "Terminated", true},
		{"recorded-crio-state", true, 3, 1, "Terminated", true},
		{"terminated-once", true, 1, 1, "Terminated", false},
		{"terminated-checkpoint", true, 3, 99, "Terminated", false},
		{"terminated-success", true, 3, 0, "Terminated", false},
		{"running-after-old-crash", true, 3, 1, "Running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, work := initialObjects("Execute")
			run.Object["status"] = map[string]interface{}{"executionPermit": map[string]interface{}{"pods": map[string]interface{}{"a-0": "pod"}}}
			cluster := object("cluster.karmada.io/v1alpha1", "Cluster")
			cluster.SetName("onpre1")
			cluster.SetUID("cluster")
			cluster.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "lastTransitionTime": stamp(now.Add(-time.Hour))}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a-0", Namespace: "ns", UID: "pod", CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "trainer", RestartCount: tc.restarts, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: tc.reason}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: tc.exit, FinishedAt: metav1.NewTime(now.Add(-time.Second))}}}}}}
			if tc.reason == "Terminated" {
				pod.Status.ContainerStatuses[0].State = pod.Status.ContainerStatuses[0].LastTerminationState
			} else if tc.reason == "Running" {
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Second))}}
			}
			if tc.name == "recorded-crio-state" {
				b, e := os.ReadFile("testdata/repeated_terminated_20261126.json")
				if e != nil {
					t.Fatal(e)
				}
				var recorded corev1.ContainerStatus
				if e = json.Unmarshal(b, &recorded); e != nil {
					t.Fatal(e)
				}
				pod.Status.ContainerStatuses[0] = recorded
				// Keep the recorded CRI state; replay after the observation time.
				now = recorded.State.Terminated.FinishedAt.Add(time.Minute)
				pod.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
			}
			c := frameworkClient(run, work, cluster)
			m := frameworkClient(pod)
			r := &RunReconciler{Client: c, Now: func() time.Time { return now }, Members: func(string) (client.Reader, error) { return m, nil }}
			rs := RunSpec{Container: "trainer", Failover: FailoverSpec{Enabled: tc.enabled}}
			got, _, err := r.confirmedSourceFailure(context.Background(), "onpre1", run, work, rs)
			if err != nil || got != tc.want {
				t.Fatal(got, err)
			}
		})
	}
}
