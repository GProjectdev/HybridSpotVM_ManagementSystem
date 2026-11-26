package framework

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type MemberReader func(string) (client.Reader, error)

// Member credentials are explicitly mounted, read-only and never logged. In
// particular AWS workers joined to onpre1 must be inspected via onpre1's API.
func MemberReaders(directory string) MemberReader {
	var cache sync.Map
	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	return func(cluster string) (client.Reader, error) {
		if !filepath.IsAbs(directory) || !valid.MatchString(cluster) {
			return nil, fmt.Errorf("explicit member kubeconfig directory required")
		}
		if c, ok := cache.Load(cluster); ok {
			return c.(client.Reader), nil
		}
		cfg, err := clientcmd.BuildConfigFromFlags("", filepath.Join(directory, cluster+".kubeconfig"))
		if err != nil {
			return nil, fmt.Errorf("member kubeconfig unavailable for %s", cluster)
		}
		cfg.Timeout = 10 * time.Second
		s := runtime.NewScheme()
		_ = corev1.AddToScheme(s)
		_ = appsv1.AddToScheme(s)
		c, err := client.New(cfg, client.Options{Scheme: s})
		if err != nil {
			return nil, err
		}
		cache.Store(cluster, c)
		return c, nil
	}
}

func configuredMembers() MemberReader {
	return MemberReaders(os.Getenv("TRAINING_MEMBER_KUBECONFIG_DIR"))
}

type WorkloadObservation struct {
	SelectedCluster                                string
	AnyPodScheduled, AllPodsScheduled, GPUShortage bool
	Running                                        bool
	Runtime                                        p.RuntimeSnapshot
	MemberWorkloadUID                              string
	ScheduledPods                                  map[string]string
	ScheduledNodes                                 map[string]string
}

// ObserveWorkload rejects stale bindings and multiple target worlds. Running is
// based on fresh per-rank telemetry, never on StatefulSet.readyReplicas alone.
func ObserveWorkload(ctx context.Context, c client.Reader, members MemberReader, run, work *unstructured.Unstructured, s RunSpec, now time.Time) (WorkloadObservation, error) {
	var result WorkloadObservation
	bindings, err := listItems(ctx, c, "work.karmada.io/v1alpha2", "ResourceBinding", run.GetNamespace())
	if err != nil {
		return result, err
	}
	var binding *unstructured.Unstructured
	for i := range bindings {
		b := &bindings[i]
		if str(b, "spec", "resource", "apiVersion") != "apps/v1" || str(b, "spec", "resource", "kind") != "StatefulSet" || str(b, "spec", "resource", "name") != work.GetName() || str(b, "spec", "resource", "namespace") != work.GetNamespace() {
			continue
		}
		if str(b, "spec", "resource", "uid") != string(work.GetUID()) || binding != nil || !b.GetDeletionTimestamp().IsZero() {
			return result, fmt.Errorf("stale or duplicate workload binding")
		}
		binding = b
	}
	if binding == nil {
		return result, nil
	}
	clusters, _, _ := unstructured.NestedSlice(binding.Object, "spec", "clusters")
	if len(clusters) > 1 {
		return result, fmt.Errorf("DDP world assigned to multiple clusters")
	}
	if len(clusters) == 1 {
		entry, ok := clusters[0].(map[string]interface{})
		if !ok {
			return result, fmt.Errorf("invalid binding cluster")
		}
		result.SelectedCluster, _ = entry["name"].(string)
		if n, exists := entry["replicas"]; exists && n != s.WorldSize {
			return result, fmt.Errorf("partial world assignment")
		}
		if result.SelectedCluster != s.OnPremiseCluster && result.SelectedCluster != s.Cloud.Cluster {
			return result, fmt.Errorf("unapproved cluster selected")
		}
	}
	// A failed scheduler decision is usable only when the reason explicitly
	// reports GPU shortage. Generic Pending, image/PVC/API errors are not proof.
	conditions, _, _ := unstructured.NestedSlice(binding.Object, "status", "conditions")
	for _, raw := range conditions {
		v, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		msg, _ := v["message"].(string)
		if v["type"] == "Scheduled" && v["status"] == "False" && strings.Contains(msg, "Insufficient nvidia.com/gpu") {
			result.GPUShortage = true
		}
	}
	if result.SelectedCluster == "" {
		return result, nil
	}
	reader, err := members(result.SelectedCluster)
	if err != nil {
		return result, err
	}
	memberWork := &appsv1.StatefulSet{}
	err = reader.Get(ctx, key(run.GetNamespace(), work.GetName()), memberWork)
	if apierrors.IsNotFound(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if memberWork.Labels["training.dcnlab.com/workload-uid"] != string(work.GetUID()) {
		return result, fmt.Errorf("member workload origin mismatch")
	}
	selector, err := metav1.LabelSelectorAsSelector(memberWork.Spec.Selector)
	if err != nil {
		return result, err
	}
	pods := &corev1.PodList{}
	if err = reader.List(ctx, pods, client.InNamespace(run.GetNamespace()), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return result, err
	}
	var scheduled int64
	livePods := map[string]string{}
	liveNodes := map[string]string{}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.UID != memberWork.UID || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if pod.Spec.NodeName != "" {
			scheduled++
			livePods[pod.Name] = string(pod.UID)
			liveNodes[pod.Name] = pod.Spec.NodeName
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable" && strings.Contains(cond.Message, "Insufficient nvidia.com/gpu") {
				result.GPUShortage = true
			}
		}
	}
	result.AnyPodScheduled = scheduled > 0
	result.AllPodsScheduled = scheduled == s.WorldSize
	result.MemberWorkloadUID = string(memberWork.UID)
	result.ScheduledPods = livePods
	result.ScheduledNodes = liveNodes
	rt := p.NewObject("TrainingRuntime")
	if err = c.Get(ctx, key(run.GetNamespace(), run.GetName()+"-runtime"), rt); apierrors.IsNotFound(err) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	if !owned(rt, run) {
		return result, fmt.Errorf("runtime ownership mismatch")
	}
	if str(rt, "spec", "sourceCluster") != result.SelectedCluster {
		_, permit, _ := unstructured.NestedMap(run.Object, "status", "executionPermit")
		if s.ExecutionGate && str(run, "status", "committedCluster") == "" && !permit {
			// Karmada selected a different initial candidate before any process
			// was admitted. The reconciler may register that candidate next.
			return result, nil
		}
		return result, fmt.Errorf("runtime member changed after execution commitment")
	}
	result.Runtime = p.ReadRuntimeStatus(rt, result.SelectedCluster)
	if result.Runtime.MemberWorkloadUID == "" {
		return result, nil
	}
	if result.Runtime.MemberWorkloadUID != memberWork.UID {
		return result, fmt.Errorf("runtime belongs to a previous member workload")
	}
	for _, rank := range result.Runtime.Pods {
		if livePods[rank.Name] != rank.UID || rank.UID == "" || rank.GlobalStep < 1 {
			return result, nil
		}
	}
	input := p.PolicyInput{WorkloadRef: p.WorkloadRef{UID: work.GetUID()}, TargetWorkers: s.WorldSize}
	result.Running = result.AllPodsScheduled && p.RuntimeReadyForCheckpoint(input, result.Runtime, now) && result.Runtime.GlobalStep > 0
	return result, nil
}
