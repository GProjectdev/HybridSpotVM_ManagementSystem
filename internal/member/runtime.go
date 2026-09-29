package member

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type RuntimeObservation struct {
	GlobalStep           int64                 `json:"globalStep"`
	CheckpointID         string                `json:"checkpointID"`
	Rank                 int64                 `json:"rank"`
	WorldSize            int64                 `json:"worldSize"`
	ObservedAt           string                `json:"observedAt"`
	State                string                `json:"state"`
	IterationTimeSeconds *float64              `json:"iterationTimeSeconds,omitempty"`
	IterationMeasurement *IterationMeasurement `json:"iterationMeasurement,omitempty"`
	WorkerSession        string                `json:"workerSession,omitempty"`
}
type RuntimeReconciler struct {
	client.Client
	HTTP *http.Client
}

func (r *RuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	o := resource.Object("TrainingRuntime")
	if err := r.Get(ctx, req.NamespacedName, o); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !o.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	status := map[string]interface{}{"observedGeneration": o.GetGeneration(), "observedAt": resource.Timestamp(time.Now()), "readyRanks": int64(0), "phase": "Collecting"}
	collectErr := r.collect(ctx, o.GetNamespace(), resource.String(o, "spec", "workloadRef", "name"), resource.String(o, "spec", "workloadRef", "uid"), resource.Int(o, "spec", "expectedWorldSize"), resource.Int(o, "spec", "port"), status)
	if collectErr != nil {
		status["phase"] = "Unavailable"
		status["message"] = collectErr.Error()
		status["readyRanks"] = int64(0)
		// Identity remains useful for fencing, but must never imply healthy ranks.
		if _, captured := status["sourcePods"]; !captured && resource.String(o, "status", "sourceWorldUID") == resource.String(o, "spec", "workloadRef", "uid") {
			if previous, ok, _ := unstructured.NestedSlice(o.Object, "status", "sourcePods"); ok {
				status["sourcePods"] = previous
				status["sourceWorldUID"] = resource.String(o, "status", "sourceWorldUID")
			}
		}
	} else {
		previous, _, _ := unstructured.NestedSlice(o.Object, "status", "pods")
		attachPreviousObservations(status["pods"].([]interface{}), previous)
	}
	if err := resource.SetStatus(ctx, r.Client, o, status); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func attachPreviousObservations(current, previous []interface{}) {
	byName := map[string]map[string]interface{}{}
	for _, raw := range previous {
		if p, ok := raw.(map[string]interface{}); ok {
			name, _ := p["name"].(string)
			byName[name] = p
		}
	}
	for _, raw := range current {
		p := raw.(map[string]interface{})
		name, _ := p["name"].(string)
		old := byName[name]
		if old == nil || p["uid"] != old["uid"] || p["rank"] != old["rank"] || p["checkpointID"] != old["checkpointID"] || p["workerSession"] != old["workerSession"] {
			continue
		}
		at, e1 := time.Parse(time.RFC3339Nano, p["observedAt"].(string))
		oldStamp, _ := old["observedAt"].(string)
		oldAt, e2 := time.Parse(time.RFC3339Nano, oldStamp)
		oldStep, ok := old["globalStep"].(int64)
		if e1 != nil || e2 != nil || !ok || oldStep < 0 || p["globalStep"].(int64) < oldStep {
			continue
		}
		if at.After(oldAt) {
			p["previousGlobalStep"] = oldStep
			p["previousObservedAt"] = oldStamp
		} else if at.Equal(oldAt) {
			step, hasStep := old["previousGlobalStep"].(int64)
			stamp, hasStamp := old["previousObservedAt"].(string)
			if hasStep && hasStamp {
				p["previousGlobalStep"], p["previousObservedAt"] = step, stamp
			}
		}
	}
}
func (r *RuntimeReconciler) collect(ctx context.Context, ns, name, originUID string, expected, port int64, status map[string]interface{}) error {
	if expected < 1 || name == "" || originUID == "" {
		return fmt.Errorf("workload identity and positive expectedWorldSize required")
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sts); err != nil {
		return err
	}
	// Karmada and Member UIDs differ; an explicit origin label binds the telemetry.
	if sts.Labels["training.dcnlab.com/workload-uid"] != originUID {
		return fmt.Errorf("workload origin UID label mismatch")
	}
	sel, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil {
		return err
	}
	pods := &corev1.PodList{}
	if err = r.List(ctx, pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return err
	}
	if identities := sourcePodIdentities(sts, pods, expected); identities != nil {
		status["sourcePods"] = identities
		status["sourceWorldUID"] = originUID
	}
	if port == 0 {
		port = 8298
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid runtime port")
	}
	seen := map[int64]bool{}
	samples := []interface{}{}
	timings := []RuntimeObservation{}
	var minStep int64
	var checkpoint string
	h := r.HTTP
	if h == nil {
		h = &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.UID != sts.UID || pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning || net.ParseIP(pod.Status.PodIP) == nil {
			return fmt.Errorf("pod %s not running", pod.Name)
		}
		endpoint := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.FormatInt(port, 10)) + "/runtime"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := h.Do(req)
		if err != nil {
			return fmt.Errorf("pod %s runtime: %w", pod.Name, err)
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 65537))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode != 200 || len(b) > 65536 {
			return fmt.Errorf("pod %s invalid runtime response", pod.Name)
		}
		var sample RuntimeObservation
		if err = json.Unmarshal(b, &sample); err != nil {
			return err
		}
		stamp, err := time.Parse(time.RFC3339Nano, sample.ObservedAt)
		if err != nil || time.Since(stamp) > 30*time.Second || time.Until(stamp) > 5*time.Second {
			return fmt.Errorf("pod %s stale runtime sample", pod.Name)
		}
		if sample.State != "Running" || sample.WorldSize != expected || sample.Rank < 0 || sample.Rank >= expected || sample.GlobalStep < 0 || seen[sample.Rank] {
			return fmt.Errorf("pod %s invalid rank membership", pod.Name)
		}
		if len(samples) == 0 {
			minStep = sample.GlobalStep
			checkpoint = sample.CheckpointID
		} else {
			if sample.GlobalStep < minStep {
				minStep = sample.GlobalStep
			}
			if checkpoint != sample.CheckpointID {
				return fmt.Errorf("ranks report different checkpoints")
			}
		}
		seen[sample.Rank] = true
		entry := map[string]interface{}{"name": pod.Name, "uid": string(pod.UID), "rank": sample.Rank, "nodeName": pod.Spec.NodeName, "globalStep": sample.GlobalStep, "checkpointID": sample.CheckpointID, "observedAt": sample.ObservedAt}
		entry["workerSession"] = sample.WorkerSession
		if validIterationMeasurement(sample, time.Now()) {
			entry["iterationMeasurement"] = sample.IterationMeasurement.status()
		}
		timings = append(timings, sample)
		samples = append(samples, entry)
	}
	if int64(len(samples)) != expected {
		return fmt.Errorf("expected %d ranks, observed %d", expected, len(samples))
	}
	status["pods"] = samples
	status["readyRanks"] = expected
	status["worldSize"] = expected
	status["globalStep"] = minStep
	status["checkpointID"] = checkpoint
	status["phase"] = "Running"
	status["workloadUID"] = originUID
	status["memberWorkloadUID"] = string(sts.UID)
	aggregateIterationTiming(status, timings, time.Now())
	return nil
}

func sourcePodIdentities(sts *appsv1.StatefulSet, pods *corev1.PodList, expected int64) []interface{} {
	byRank := map[int64]interface{}{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.UID != sts.UID {
			continue
		}
		rank, err := strconv.ParseInt(strings.TrimPrefix(pod.Name, sts.Name+"-"), 10, 64)
		if err != nil || rank < 0 || rank >= expected || pod.Name != fmt.Sprintf("%s-%d", sts.Name, rank) || pod.UID == "" || pod.Spec.NodeName == "" || byRank[rank] != nil {
			return nil
		}
		byRank[rank] = map[string]interface{}{"name": pod.Name, "uid": string(pod.UID), "nodeName": pod.Spec.NodeName, "rank": rank}
	}
	if int64(len(byRank)) != expected {
		return nil
	}
	out := make([]interface{}, 0, expected)
	for rank := int64(0); rank < expected; rank++ {
		out = append(out, byRank[rank])
	}
	return out
}
func SetupRuntime(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("training-runtime").WithEventFilter(predicate.GenerationChangedPredicate{}).For(resource.Object("TrainingRuntime")).Complete(&RuntimeReconciler{Client: mgr.GetClient()})
}
