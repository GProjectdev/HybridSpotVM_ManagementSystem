package policy

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPolicyInputRequiresBoundVerifiedPlacement(t *testing.T) {
	for _, mode := range []string{"valid", "policy UID", "workload UID", "initial cluster", "initial runtime", "unverified", "empty cluster", "empty runtime"} {
		t.Run(mode, func(t *testing.T) {
			obj := NewObject("TrainingPolicy")
			obj.SetName("train")
			obj.SetUID("policy-uid")
			obj.Object["spec"] = map[string]interface{}{
				"sourceCluster": "onprem", "workloadRef": map[string]interface{}{"uid": "workload-uid"},
			}
			placement := map[string]interface{}{
				"policyUID": "policy-uid", "workloadUID": "workload-uid",
				"initialSourceCluster": "onprem", "initialRuntimeName": "train-runtime",
				"activeCluster": "aws", "runtimeRef": map[string]interface{}{"name": "aws-runtime"}, "verified": true,
			}
			switch mode {
			case "policy UID":
				placement["policyUID"] = "old"
			case "workload UID":
				placement["workloadUID"] = "old"
			case "initial cluster":
				placement["initialSourceCluster"] = "old"
			case "initial runtime":
				placement["initialRuntimeName"] = "old"
			case "unverified":
				placement["verified"] = false
			case "empty cluster":
				placement["activeCluster"] = ""
			case "empty runtime":
				placement["runtimeRef"] = map[string]interface{}{}
			}
			_ = unstructured.SetNestedMap(obj.Object, placement, "status", "placement")
			input := ReadPolicyInput(obj)
			if mode == "valid" {
				if input.SourceCluster != "aws" || input.RuntimeRefName != "aws-runtime" {
					t.Fatal("valid placement ignored")
				}
			} else if input.SourceCluster != "onprem" || input.RuntimeRefName != "train-runtime" {
				t.Fatal("stale/unverified placement adopted")
			}
			if ReadPolicySpec(obj).SourceCluster != "onprem" {
				t.Fatal("intent changed")
			}
		})
	}
}
