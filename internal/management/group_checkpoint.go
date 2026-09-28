package management

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A recovery round is indivisible: never combine archives from different CRs.
type groupCheckpoint struct {
	Object      *unstructured.Unstructured
	Pods        []interface{}
	CompletedAt time.Time
}

func selectGroupCheckpoint(ctx context.Context, reader client.Reader, input p.PolicyInput) (*groupCheckpoint, error) {
	list := p.NewList("FluidCRMigration")
	if err := reader.List(ctx, list, client.InNamespace(input.Namespace), client.MatchingLabels{p.LabelPolicyUID: string(input.PolicyUID)}); err != nil {
		return nil, err
	}
	var candidates []*groupCheckpoint
	for i := range list.Items {
		if round, err := readGroupCheckpoint(&list.Items[i], input); err == nil {
			candidates = append(candidates, round)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no complete durable full-group checkpoint for workload UID %s", input.WorkloadRef.UID)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CompletedAt.Equal(candidates[j].CompletedAt) {
			return candidates[i].Object.GetName() < candidates[j].Object.GetName()
		}
		return candidates[i].CompletedAt.After(candidates[j].CompletedAt)
	})
	return candidates[0], nil
}

func readGroupCheckpoint(cp *unstructured.Unstructured, input p.PolicyInput) (*groupCheckpoint, error) {
	if input.PolicyUID == "" || input.WorkloadRef.UID == "" || input.SourceCluster == "" || input.Namespace == "" ||
		cp.GetNamespace() != input.Namespace || cp.GetUID() == "" || cp.GetGeneration() < 1 || !cp.GetDeletionTimestamp().IsZero() || input.TargetWorkers < 1 || cp.GetLabels()[p.LabelPolicyUID] != string(input.PolicyUID) {
		return nil, fmt.Errorf("checkpoint identity or policy ownership mismatch")
	}
	ref := input.WorkloadRef
	if stringField(cp.Object, "spec", "workloadRef", "uid") != string(ref.UID) || stringField(cp.Object, "spec", "workloadRef", "name") != ref.Name || stringField(cp.Object, "spec", "workloadRef", "kind") != "StatefulSet" || stringField(cp.Object, "spec", "workloadRef", "apiVersion") != "apps/v1" {
		return nil, fmt.Errorf("checkpoint workload provenance mismatch")
	}
	if partial, found, _ := unstructured.NestedMap(cp.Object, "spec", "partialCheckpoint"); found && len(partial) > 0 {
		return nil, fmt.Errorf("partial checkpoint cannot recover a whole group")
	}
	roundID := cp.GetAnnotations()["training.dcnlab.com/checkpoint-id"]
	if roundID == "" {
		return nil, fmt.Errorf("checkpoint round ID missing")
	}
	var report map[string]interface{}
	for _, cluster := range nestedClusterStatuses(cp.Object) {
		if stringField(cluster, "clusterName") != input.SourceCluster {
			continue
		}
		if report != nil {
			return nil, fmt.Errorf("duplicate source checkpoint report")
		}
		report = cluster
	}
	if report == nil || intField(report, "observedGeneration") != cp.GetGeneration() || stringField(report, "phase") != "Completed" {
		return nil, fmt.Errorf("current source checkpoint completion required")
	}
	completed, err := time.Parse(time.RFC3339, stringField(report, "completionTime"))
	if err != nil {
		return nil, fmt.Errorf("checkpoint completion time required")
	}
	pods, ok := mapSliceFromStatus(report, "pods")
	if !ok || int64(len(pods)) != input.TargetWorkers {
		return nil, fmt.Errorf("checkpoint rank count mismatch")
	}
	byName := make(map[string]map[string]interface{}, len(pods))
	seenUID := map[string]bool{}
	for _, pod := range pods {
		name, uid := stringField(pod, "podName"), stringField(pod, "podUID")
		if name == "" || uid == "" || seenUID[uid] || byName[name] != nil || stringField(pod, "nodeName") == "" {
			return nil, fmt.Errorf("checkpoint has missing or duplicate source identity")
		}
		phase := stringField(pod, "phase")
		if phase != "Resumed" && phase != "ContainerCheckpointed" {
			return nil, fmt.Errorf("checkpoint Pod is not complete")
		}
		if id := stringField(pod, "checkpointID"); id != "" && id != roundID {
			return nil, fmt.Errorf("mixed checkpoint rounds")
		}
		byName[name], seenUID[uid] = pod, true
	}
	result := &groupCheckpoint{Object: cp.DeepCopy(), CompletedAt: completed}
	for rank := int64(0); rank < input.TargetWorkers; rank++ {
		name := fmt.Sprintf("%s-%d", ref.Name, rank)
		pod := byName[name]
		if pod == nil {
			return nil, fmt.Errorf("checkpoint missing ordinal %d", rank)
		}
		files, ok := mapSliceFromStatus(pod, "checkpointFiles")
		if !ok || len(files) != 1 {
			return nil, fmt.Errorf("one exported trainer archive per rank required")
		}
		file := files[0]
		sha := stringField(file, "sha256")
		digest, err := hex.DecodeString(sha)
		if err != nil || len(digest) != 32 || strings.ToLower(sha) != sha {
			return nil, fmt.Errorf("invalid archive digest")
		}
		if stringField(file, "containerName") == "" || stringField(file, "filePath") == "" || stringField(file, "durableRef") != "file-store:"+cp.GetNamespace()+"/sha256/"+sha {
			return nil, fmt.Errorf("archive export identity mismatch")
		}
		if _, err := time.Parse(time.RFC3339, stringField(file, "exportedAt")); err != nil {
			return nil, fmt.Errorf("archive export not confirmed")
		}
		if id := stringField(file, "checkpointID"); id != "" && id != roundID {
			return nil, fmt.Errorf("mixed archive round")
		}
		result.Pods = append(result.Pods, map[string]interface{}{
			"rank": rank, "sourcePod": name, "sourcePodUID": stringField(pod, "podUID"), "sourceNode": stringField(pod, "nodeName"), "targetPod": name,
			"archives": []interface{}{map[string]interface{}{
				"containerName": stringField(file, "containerName"), "sourcePath": stringField(file, "filePath"), "targetPath": "/var/lib/kubelet/checkpoints/" + sha + ".tar", "sha256": sha, "durableRef": stringField(file, "durableRef"),
			}},
		})
	}
	return result, nil
}
