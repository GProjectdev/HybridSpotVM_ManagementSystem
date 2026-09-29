package manifests_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structural "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestCRDsAreStructuralWithStatusSubresources(t *testing.T) {
	files, err := filepath.Glob("../../config/crd/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var crd extv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatal(err)
		}
		if crd.Kind != "CustomResourceDefinition" {
			continue
		}
		found[crd.Spec.Names.Kind] = true
		t.Run(crd.Spec.Names.Kind, func(t *testing.T) {
			extv1.SetDefaults_CustomResourceDefinition(&crd)
			full := &ext.CustomResourceDefinition{}
			if err := extv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, full, nil); err != nil {
				t.Fatal(err)
			}
			full.Status.StoredVersions = []string{crd.Spec.Versions[0].Name}
			if errs := apivalidation.ValidateCustomResourceDefinition(context.Background(), full); len(errs) != 0 {
				t.Fatalf("CRD admission including CEL: %v", errs.ToAggregate())
			}
			if crd.Spec.Scope != extv1.NamespaceScoped {
				t.Fatal("training CRs must be namespaced")
			}
			if len(crd.Spec.Versions) != 1 {
				t.Fatal("expected one served version")
			}
			v := crd.Spec.Versions[0]
			if !v.Served || !v.Storage || v.Subresources == nil || v.Subresources.Status == nil {
				t.Fatal("missing version/status contract")
			}
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				t.Fatal("missing schema")
			}
			internal := &ext.JSONSchemaProps{}
			if err := extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v.Schema.OpenAPIV3Schema, internal, nil); err != nil {
				t.Fatal(err)
			}
			s, err := structural.NewStructural(internal)
			if err != nil {
				t.Fatal(err)
			}
			if errs := structural.ValidateStructural(field.NewPath("schema"), s); len(errs) != 0 {
				t.Fatal(errs.ToAggregate())
			}
			if crd.Spec.Names.Kind == "TrainingPolicy" {
				economics := v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["policy"].Properties["economics"]
				if !reflect.DeepEqual(economics.Required, []string{"enabled"}) {
					t.Fatal("automatic recovery calibration must not require manual cost/timestamp")
				}
				obj := map[string]interface{}{"spec": map[string]interface{}{
					"policy": map[string]interface{}{"economics": map[string]interface{}{
						"enabled": true, "lossCostPerEviction": float64(12.5),
						"observedAt": "2026-09-27T00:00:00Z", "maxAgeSeconds": int64(600),
					}},
				}}
				obj["status"] = map[string]interface{}{"policy": map[string]interface{}{
					"capacityOwnedWorkers": int64(3), "capacityTargetWorkers": int64(2),
					"capacityOvershoot": true, "replacementRequired": true,
					"replacementOperation": "replacement-1", "replacementNodeProvision": "worker-new",
					"decisionStage": "Bootstrap", "intervalSource": "risk-band-bootstrap",
					"priceEvaluated": true, "economicsSource": "operator-calibration",
				}}
				obj["status"].(map[string]interface{})["placement"] = map[string]interface{}{
					"policyUID": "policy-uid", "workloadUID": "workload-uid",
					"initialSourceCluster": "onprem", "initialRuntimeName": "trainer-runtime",
					"activeCluster": "aws", "runtimeRef": map[string]interface{}{"name": "trainer-rt-aws"},
					"verified": true, "verifiedAt": "2026-09-29T00:00:00Z",
				}
				obj["status"].(map[string]interface{})["discovery"] = map[string]interface{}{
					"ready": true, "observedGeneration": int64(1), "workloadUID": "workload-uid",
					"runtimeRef": map[string]interface{}{"name": "trainer-runtime"},
				}
				before := runtime.DeepCopyJSON(obj)
				pruning.Prune(obj, s, true)
				if !reflect.DeepEqual(before, obj) {
					t.Fatalf("API prunes TrainingPolicy contract: before=%v after=%v", before, obj)
				}
			}
			if crd.Spec.Names.Kind == "TrainingRuntime" {
				obj := map[string]interface{}{"status": map[string]interface{}{
					"phase": "Running", "message": "", "observedGeneration": int64(2),
					"observedAt": "2026-09-26T00:00:00Z", "workloadUID": "mgmt-uid", "memberWorkloadUID": "member-uid",
					"readyRanks": int64(2), "worldSize": int64(2), "globalStep": int64(10), "checkpointID": "round-1",
					"pods":     []interface{}{map[string]interface{}{"name": "train-0", "uid": "pod-uid", "nodeName": "worker-0", "rank": int64(0), "globalStep": int64(10), "checkpointID": "round-1", "observedAt": "2026-09-26T00:00:00Z"}},
					"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "status": map[string]interface{}{"pods": []interface{}{map[string]interface{}{"name": "train-0", "uid": "pod-uid", "nodeName": "worker-0", "rank": int64(0), "globalStep": int64(10), "checkpointID": "round-1", "observedAt": "2026-09-26T00:00:00Z"}}}}},
				}}
				before := runtime.DeepCopyJSON(obj)
				pruning.Prune(obj, s, true)
				if !reflect.DeepEqual(before, obj) {
					t.Fatalf("API prunes runtime evidence: before=%v after=%v", before, obj)
				}
				status := obj["status"].(map[string]interface{})
				metric := map[string]interface{}{"method": "optimizer-update-window-v1", "aggregation": "max-rank-mean", "startStep": int64(20), "endStep": int64(40), "samples": int64(20), "rankCount": int64(2), "observedAt": "2026-09-29T00:00:00Z"}
				status["iterationTimeSeconds"], status["iterationMeasurement"] = 1.5, metric
				status["pods"].([]interface{})[0].(map[string]interface{})["workerSession"] = "worker-1"
				status["pods"].([]interface{})[0].(map[string]interface{})["iterationMeasurement"] = metric
				status["pods"].([]interface{})[0].(map[string]interface{})["survivorResume"] = map[string]interface{}{
					"checkpointID": "partial-1", "generation": int64(123), "podUID": "pod-uid",
					"rank": int64(0), "nodeName": "worker-0", "resumedAt": "2026-09-29T00:00:00Z",
				}
				memberStatus := runtime.DeepCopyJSON(status)
				delete(memberStatus, "clusters")
				status["clusters"].([]interface{})[0].(map[string]interface{})["status"] = memberStatus
				before = runtime.DeepCopyJSON(obj)
				pruning.Prune(obj, s, true)
				if !reflect.DeepEqual(before, obj) {
					t.Fatalf("API prunes iteration measurement: before=%v after=%v", before, obj)
				}
			}
			if crd.Spec.Names.Kind == "SpotRiskProfile" {
				obj := map[string]interface{}{"spec": map[string]interface{}{
					"staticLambdaPerHour": float64(0), "provider": "AWS", "region": "ap-northeast-2", "availabilityZone": "ap-northeast-2c", "instanceType": "g5.xlarge",
				}, "status": map[string]interface{}{"ready": false, "observedGeneration": int64(2), "error": "risk feed expired"}}
				before := runtime.DeepCopyJSON(obj)
				pruning.Prune(obj, s, true)
				if !reflect.DeepEqual(before, obj) {
					t.Fatalf("API prunes risk contract: before=%v after=%v", before, obj)
				}
			}
			if crd.Spec.Names.Kind == "SpotRecovery" {
				obj := map[string]interface{}{
					"spec": map[string]interface{}{
						"policyRef":                   map[string]interface{}{"name": "policy", "uid": "policy-uid", "generation": int64(3)},
						"requestUID":                  "operation-uid",
						"operation":                   "replace-spot-worker",
						"sourceCluster":               "aws",
						"targetCluster":               "onprem",
						"workloadRef":                 map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload-uid"},
						"trainingRuntimeRef":          map[string]interface{}{"name": "runtime", "uid": "runtime-uid"},
						"checkpointID":                "ckpt-1",
						"eventID":                     "event-1",
						"restoreRequestRef":           map[string]interface{}{"name": "restore", "uid": "restore-uid", "generation": int64(7)},
						"oldNodeProvisionRef":         map[string]interface{}{"name": "old", "uid": "old-uid"},
						"replacementNodeProvisionRef": map[string]interface{}{"name": "new", "uid": "new-uid"},
					},
					"status": map[string]interface{}{
						"phase":                      "Completed",
						"observedGeneration":         int64(1),
						"verifiedCheckpointID":       "ckpt-1",
						"verifiedAt":                 "2026-09-26T00:00:00Z",
						"replacementMode":            "restore-verified-only",
						"silentGenerationAutoDelete": false,
						"message":                    "old NodeProvision deleted",
					},
				}
				before := runtime.DeepCopyJSON(obj)
				pruning.Prune(obj, s, true)
				if !reflect.DeepEqual(before, obj) {
					t.Fatalf("API prunes recovery contract: before=%v after=%v", before, obj)
				}
			}
		})
	}
	for _, kind := range []string{"TrainingPolicy", "SpotRiskProfile", "TrainingRuntime", "SpotRecovery", "SpotReplacement"} {
		if !found[kind] {
			t.Errorf("missing CRD %s", kind)
		}
	}
}
