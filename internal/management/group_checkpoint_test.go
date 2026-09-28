package management

import (
	"context"
	"strings"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func groupCheckpointFixture() (*unstructured.Unstructured, p.PolicyInput) {
	input := p.PolicyInput{Namespace: "demo", PolicyUID: types.UID("policy"), SourceCluster: "aws", TargetWorkers: 2, WorkloadRef: p.WorkloadRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "trainer", UID: types.UID("workload")}}
	cp := p.NewObject("FluidCRMigration")
	cp.SetNamespace(input.Namespace)
	cp.SetName("round")
	cp.SetUID("checkpoint")
	cp.SetGeneration(1)
	cp.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID)})
	cp.SetAnnotations(map[string]string{"training.dcnlab.com/checkpoint-id": "round"})
	cp.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload"}}
	pods := []interface{}{}
	for _, name := range []string{"trainer-0", "trainer-1"} {
		sha := strings.Repeat("a", 64)
		pods = append(pods, map[string]interface{}{"podName": name, "podUID": name + "-uid", "nodeName": name + "-node", "phase": "Resumed", "checkpointFiles": []interface{}{map[string]interface{}{"containerName": "trainer", "filePath": "/var/lib/kubelet/checkpoints/source.tar", "sha256": sha, "durableRef": "file-store:demo/sha256/" + sha, "exportedAt": "2026-09-28T00:01:00Z"}}})
	}
	cp.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "observedGeneration": int64(1), "phase": "Completed", "completionTime": "2026-09-28T00:00:00Z", "pods": pods}}}
	return cp, input
}

func TestGroupCheckpointRequiresWholeDurableRound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{"valid", func(*unstructured.Unstructured) {}},
		{"wrong-namespace", func(cp *unstructured.Unstructured) { cp.SetNamespace("other") }},
		{"wrong-workload", func(cp *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(cp.Object, "other", "spec", "workloadRef", "uid")
		}},
		{"partial", func(cp *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(cp.Object, map[string]interface{}{"targetRanks": []interface{}{int64(1)}}, "spec", "partialCheckpoint")
		}},
		{"old-generation", func(cp *unstructured.Unstructured) { cp.SetGeneration(2) }},
		{"failed", func(cp *unstructured.Unstructured) { groupReport(cp)["phase"] = "Failed" }},
		{"missing-rank", func(cp *unstructured.Unstructured) {
			groupReport(cp)["pods"] = groupReport(cp)["pods"].([]interface{})[:1]
		}},
		{"duplicate-rank", func(cp *unstructured.Unstructured) {
			pods := groupReport(cp)["pods"].([]interface{})
			pods[1] = pods[0]
		}},
		{"missing-export", func(cp *unstructured.Unstructured) { delete(groupFile(cp), "exportedAt") }},
		{"invalid-digest", func(cp *unstructured.Unstructured) { groupFile(cp)["sha256"] = "../escape" }},
		{"wrong-store", func(cp *unstructured.Unstructured) {
			groupFile(cp)["durableRef"] = "file-store:other/sha256/" + strings.Repeat("a", 64)
		}},
		{"mixed-round", func(cp *unstructured.Unstructured) { groupFile(cp)["checkpointID"] = "other-round" }},
		{"duplicate-source", func(cp *unstructured.Unstructured) {
			clusters := cp.Object["status"].(map[string]interface{})["clusters"].([]interface{})
			cp.Object["status"].(map[string]interface{})["clusters"] = append(clusters, clusters[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, input := groupCheckpointFixture()
			tc.mutate(cp)
			round, err := readGroupCheckpoint(cp, input)
			if tc.name == "valid" {
				if err != nil || len(round.Pods) != 2 {
					t.Fatalf("round=%v err=%v", round, err)
				}
			} else if err == nil {
				t.Fatal("unsafe checkpoint accepted")
			}
		})
	}
}

func TestGroupCheckpointFallsBackWithoutMixingRounds(t *testing.T) {
	cp, input := groupCheckpointFixture()
	newer := cp.DeepCopy()
	newer.SetName("newer")
	newer.SetUID("newer-uid")
	groupReport(newer)["completionTime"] = "2026-09-28T01:00:00Z"
	delete(groupFile(newer), "exportedAt")
	r := checkpointReconcilerFixture(t, func() time.Time { return time.Now() }, cp, newer)
	round, err := selectGroupCheckpoint(context.Background(), r.Client, input)
	if err != nil || round.Object.GetName() != "round" {
		t.Fatalf("round=%v err=%v", round, err)
	}
	if err := r.Delete(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	if _, err := selectGroupCheckpoint(context.Background(), r.Client, input); err == nil {
		t.Fatal("accepted incomplete latest round")
	}
}

func groupReport(cp *unstructured.Unstructured) map[string]interface{} {
	return cp.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})
}
func groupFile(cp *unstructured.Unstructured) map[string]interface{} {
	return groupReport(cp)["pods"].([]interface{})[0].(map[string]interface{})["checkpointFiles"].([]interface{})[0].(map[string]interface{})
}
