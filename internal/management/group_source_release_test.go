package management

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func groupSourceReleaseFixture(t *testing.T) (*unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	r, p := groupReleaseFixture(true)
	for _, obj := range []*unstructured.Unstructured{r, p} {
		group := obj.Object["spec"].(map[string]interface{})["groupRestore"].(map[string]interface{})
		group["worldSize"] = int64(2)
		group["sourcePods"] = append(group["sourcePods"].([]interface{}), map[string]interface{}{
			"rank": int64(1), "podName": "trainer-1", "podUID": "CURRENT-1", "nodeName": "old-1",
		})
	}
	_ = unstructured.SetNestedField(r.Object, "Preparing", "status", "phase")
	unstructured.RemoveNestedField(r.Object, "status", "groupControl")
	report := targetReport(p)
	report["clusterName"] = "onprem"
	report["phase"] = "SourceFenced"
	report["groupControl"] = map[string]interface{}{"volumeServer": "nfs", "volumePath": "/shared"}
	second := runtime.DeepCopyJSONValue(report["sourceFences"].([]interface{})[0]).(map[string]interface{})
	second["podName"], second["sourcePodUID"] = "trainer-1", "CURRENT-1"
	report["sourceFences"] = append(report["sourceFences"].([]interface{}), second)
	mutateSourceReceipt(t, p, func(receipt map[string]interface{}) { receipt["fences"] = report["sourceFences"] })
	return r, p
}

func mutateSourceReceipt(t *testing.T, plan *unstructured.Unstructured, mutate func(map[string]interface{})) {
	t.Helper()
	var receipt map[string]interface{}
	if err := json.Unmarshal([]byte(plan.GetAnnotations()[groupFenceAnnotation]), &receipt); err != nil {
		t.Fatal(err)
	}
	mutate(receipt)
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	plan.SetAnnotations(map[string]string{groupFenceAnnotation: string(encoded)})
}

func sourceReleaseFence(p *unstructured.Unstructured) map[string]interface{} {
	return targetReport(p)["sourceFences"].([]interface{})[0].(map[string]interface{})
}

func TestGroupSourceFenceBeforeTargetPreparation(t *testing.T) {
	for _, withTarget := range []bool{false, true} {
		r, p := groupSourceReleaseFixture(t)
		if withTarget {
			clusters := p.Object["status"].(map[string]interface{})["clusters"].([]interface{})
			p.Object["status"].(map[string]interface{})["clusters"] = append(clusters, map[string]interface{}{
				"clusterName": "aws", "observedGeneration": p.GetGeneration(), "phase": "Pending",
			})
		}
		beforeRequest, beforePlan := r.DeepCopy(), p.DeepCopy()
		reader := fake.NewClientBuilder().WithObjects(p).Build()
		if err := reader.Get(context.Background(), client.ObjectKeyFromObject(p), beforePlan); err != nil {
			t.Fatal(err)
		}
		if err := validateGroupSourceFence(context.Background(), reader, r); err != nil {
			t.Fatalf("withTarget=%v: %v", withTarget, err)
		}
		stored := p.DeepCopy()
		if err := reader.Get(context.Background(), client.ObjectKeyFromObject(p), stored); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(beforeRequest.Object, r.Object) || !reflect.DeepEqual(beforePlan.Object, stored.Object) {
			t.Fatal("source validation mutated request or plan")
		}
		if err := validateGroupRelease(context.Background(), reader, r); err == nil {
			t.Fatal("source fence alone authorized final release")
		}
		_ = unstructured.SetNestedField(r.Object, "Prepared", "status", "phase")
		if err := validateGroupRelease(context.Background(), reader, r); err == nil {
			t.Fatal("request Prepared phase bypassed target preparation evidence")
		}
	}
}

func TestGroupSourceFenceAcceptsReorderedReportsAndTypedDefaults(t *testing.T) {
	r, p := groupSourceReleaseFixture(t)
	r.Object["spec"].(map[string]interface{})["pods"].([]interface{})[0].(map[string]interface{})["rank"] = int64(0)
	p.Object["spec"].(map[string]interface{})["localPodRestore"] = false
	fences := targetReport(p)["sourceFences"].([]interface{})
	fences[0], fences[1] = fences[1], fences[0]
	if err := validateGroupSourceFence(context.Background(), fake.NewClientBuilder().WithObjects(p).Build(), r); err != nil {
		t.Fatal(err)
	}
}

func TestGroupSourceFenceRequestPhases(t *testing.T) {
	allowed := map[string]bool{
		"AwaitingSourceFence": true, "Preparing": true, "Prepared": true, "Running": true, "Verified": true,
	}
	for _, phase := range []string{"AwaitingSourceFence", "Preparing", "Prepared", "Running", "Verified", "Failed", "Cancelled", "Canceled", "Aborted", "Pending", "SourceFenced", "AwaitingArtifacts", "SourceFencing", "Resuming", "", "Unknown"} {
		t.Run(phase, func(t *testing.T) {
			r, p := groupSourceReleaseFixture(t)
			_ = unstructured.SetNestedField(r.Object, phase, "status", "phase")
			err := validateGroupSourceFence(context.Background(), fake.NewClientBuilder().WithObjects(p).Build(), r)
			if (err == nil) != allowed[phase] {
				t.Fatalf("phase=%q allowed=%v: %v", phase, allowed[phase], err)
			}
		})
	}
}

func TestGroupSourceFenceRejectsUnboundEvidence(t *testing.T) {
	cases := map[string]func(*unstructured.Unstructured, *unstructured.Unstructured){
		"request UID missing":        func(r, p *unstructured.Unstructured) { r.SetUID("") },
		"request generation missing": func(r, p *unstructured.Unstructured) { r.SetGeneration(0) },
		"request deleting":           func(r, p *unstructured.Unstructured) { now := metav1.Now(); r.SetDeletionTimestamp(&now) },
		"request stale": func(r, p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(r.Object, int64(1), "status", "observedGeneration")
		},
		"plan name missing":       func(r, p *unstructured.Unstructured) { unstructured.RemoveNestedField(r.Object, "status", "planName") },
		"plan not found":          func(r, p *unstructured.Unstructured) { p.SetName("other") },
		"plan namespace mismatch": func(r, p *unstructured.Unstructured) { p.SetNamespace("other") },
		"plan UID missing":        func(r, p *unstructured.Unstructured) { p.SetUID("") },
		"plan generation missing": func(r, p *unstructured.Unstructured) { p.SetGeneration(0) },
		"plan deleting": func(r, p *unstructured.Unstructured) {
			now := metav1.Now()
			p.SetDeletionTimestamp(&now)
			p.SetFinalizers([]string{"test"})
		},
		"plan owner missing": func(r, p *unstructured.Unstructured) { p.SetOwnerReferences(nil) },
		"plan owner UID mismatch": func(r, p *unstructured.Unstructured) {
			owners := p.GetOwnerReferences()
			owners[0].UID = "old-request"
			p.SetOwnerReferences(owners)
		},
		"plan owner not controller": func(r, p *unstructured.Unstructured) {
			owners := p.GetOwnerReferences()
			no := false
			owners[0].Controller = &no
			p.SetOwnerReferences(owners)
		},
		"plan request UID mismatch": func(r, p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "old-request", "spec", "requestUID")
		},
		"plan spec drift": func(r, p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "other", "spec", "checkpointRef", "checkpointID")
		},
		"request spec missing":  func(r, p *unstructured.Unstructured) { delete(r.Object, "spec") },
		"source report missing": func(r, p *unstructured.Unstructured) { delete(p.Object, "status") },
		"wrong source cluster":  func(r, p *unstructured.Unstructured) { targetReport(p)["clusterName"] = "aws" },
		"duplicate source": func(r, p *unstructured.Unstructured) {
			report := targetReport(p)
			p.Object["status"].(map[string]interface{})["clusters"] = []interface{}{report, runtime.DeepCopyJSONValue(report)}
		},
		"source report stale":    func(r, p *unstructured.Unstructured) { targetReport(p)["observedGeneration"] = int64(2) },
		"source report pending":  func(r, p *unstructured.Unstructured) { targetReport(p)["phase"] = "Fencing" },
		"source report failed":   func(r, p *unstructured.Unstructured) { targetReport(p)["phase"] = "Failed" },
		"source control missing": func(r, p *unstructured.Unstructured) { delete(targetReport(p), "groupControl") },
		"source volume server missing": func(r, p *unstructured.Unstructured) {
			delete(targetReport(p)["groupControl"].(map[string]interface{}), "volumeServer")
		},
		"source volume path missing": func(r, p *unstructured.Unstructured) {
			delete(targetReport(p)["groupControl"].(map[string]interface{}), "volumePath")
		},
		"receipt missing":       func(r, p *unstructured.Unstructured) { p.SetAnnotations(nil) },
		"receipt malformed":     func(r, p *unstructured.Unstructured) { p.SetAnnotations(map[string]string{groupFenceAnnotation: "{"}) },
		"source fences missing": func(r, p *unstructured.Unstructured) { delete(targetReport(p), "sourceFences") },
		"source fences partial": func(r, p *unstructured.Unstructured) {
			targetReport(p)["sourceFences"] = targetReport(p)["sourceFences"].([]interface{})[:1]
		},
		"source fences duplicated": func(r, p *unstructured.Unstructured) {
			fences := targetReport(p)["sourceFences"].([]interface{})
			fences[1] = runtime.DeepCopyJSONValue(fences[0])
		},
		"source historical UID":    func(r, p *unstructured.Unstructured) { sourceReleaseFence(p)["sourcePodUID"] = "HISTORICAL" },
		"source fence stale":       func(r, p *unstructured.Unstructured) { sourceReleaseFence(p)["observedGeneration"] = int64(2) },
		"source fence not gone":    func(r, p *unstructured.Unstructured) { sourceReleaseFence(p)["phase"] = "Deleting" },
		"source timestamp invalid": func(r, p *unstructured.Unstructured) { sourceReleaseFence(p)["goneObservedAt"] = "invalid" },
		"source timestamps reversed": func(r, p *unstructured.Unstructured) {
			sourceReleaseFence(p)["goneObservedAt"] = "2026-09-27T00:00:00Z"
		},
		"source delete timestamp differs": func(r, p *unstructured.Unstructured) {
			sourceReleaseFence(p)["deleteRequestedAt"] = "2026-09-28T00:00:01Z"
		},
		"source gone timestamp differs": func(r, p *unstructured.Unstructured) {
			sourceReleaseFence(p)["goneObservedAt"] = "2026-09-28T00:00:11Z"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, p := groupSourceReleaseFixture(t)
			mutate(r, p)
			if err := validateGroupSourceFence(context.Background(), fake.NewClientBuilder().WithObjects(p).Build(), r); err == nil {
				t.Fatal("unsafe source fence accepted")
			}
		})
	}
}

func TestGroupSourceFenceRejectsInvalidReceipt(t *testing.T) {
	for _, field := range []string{"requestUID", "operationUID", "sourceWorldUID", "sourceCluster", "volumeServer", "volumePath"} {
		t.Run(field, func(t *testing.T) {
			r, p := groupSourceReleaseFixture(t)
			mutateSourceReceipt(t, p, func(receipt map[string]interface{}) { receipt[field] = "wrong" })
			if err := validateGroupSourceFence(context.Background(), fake.NewClientBuilder().WithObjects(p).Build(), r); err == nil {
				t.Fatal("mismatched receipt accepted")
			}
		})
	}
	cases := map[string]func(map[string]interface{}){
		"missing":   func(r map[string]interface{}) { delete(r, "fences") },
		"partial":   func(r map[string]interface{}) { r["fences"] = r["fences"].([]interface{})[:1] },
		"duplicate": func(r map[string]interface{}) { f := r["fences"].([]interface{}); f[1] = f[0] },
		"historical UID": func(r map[string]interface{}) {
			r["fences"].([]interface{})[0].(map[string]interface{})["sourcePodUID"] = "HISTORICAL"
		},
		"stale generation": func(r map[string]interface{}) {
			r["fences"].([]interface{})[0].(map[string]interface{})["observedGeneration"] = float64(2)
		},
		"fractional generation": func(r map[string]interface{}) {
			r["fences"].([]interface{})[0].(map[string]interface{})["observedGeneration"] = 3.5
		},
		"not gone": func(r map[string]interface{}) {
			r["fences"].([]interface{})[0].(map[string]interface{})["phase"] = "Deleting"
		},
		"missing timestamp": func(r map[string]interface{}) {
			delete(r["fences"].([]interface{})[0].(map[string]interface{}), "goneObservedAt")
		},
		"reversed timestamps": func(r map[string]interface{}) {
			r["fences"].([]interface{})[0].(map[string]interface{})["goneObservedAt"] = "2026-09-27T00:00:00Z"
		},
		"different timestamp": func(r map[string]interface{}) {
			r["fences"].([]interface{})[0].(map[string]interface{})["goneObservedAt"] = "2026-09-28T00:00:12Z"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, p := groupSourceReleaseFixture(t)
			mutateSourceReceipt(t, p, mutate)
			if err := validateGroupSourceFence(context.Background(), fake.NewClientBuilder().WithObjects(p).Build(), r); err == nil {
				t.Fatal("invalid receipt fences accepted")
			}
		})
	}
}

func TestGroupSourceFenceRejectsInvalidGroupSpec(t *testing.T) {
	cases := map[string]func(map[string]interface{}){
		"self asserted":          func(s map[string]interface{}) { s["sourceFenced"] = true },
		"malformed assertion":    func(s map[string]interface{}) { s["sourceFenced"] = "false" },
		"missing source cluster": func(s map[string]interface{}) { s["sourceCluster"] = "" },
		"missing group":          func(s map[string]interface{}) { delete(s, "groupRestore") },
		"missing operation":      func(s map[string]interface{}) { s["groupRestore"].(map[string]interface{})["operationUID"] = "" },
		"wrong world":            func(s map[string]interface{}) { s["groupRestore"].(map[string]interface{})["sourceWorldUID"] = "wrong" },
		"empty world": func(s map[string]interface{}) {
			s["groupRestore"].(map[string]interface{})["sourceWorldUID"] = ""
			s["workloadRef"].(map[string]interface{})["uid"] = ""
		},
		"incomplete world": func(s map[string]interface{}) { s["groupRestore"].(map[string]interface{})["worldSize"] = int64(3) },
		"empty sources": func(s map[string]interface{}) {
			s["groupRestore"].(map[string]interface{})["sourcePods"] = []interface{}{}
		},
		"duplicate source": func(s map[string]interface{}) {
			pods := s["groupRestore"].(map[string]interface{})["sourcePods"].([]interface{})
			pods[1] = runtime.DeepCopyJSONValue(pods[0])
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, p := groupSourceReleaseFixture(t)
			for _, obj := range []*unstructured.Unstructured{r, p} {
				mutate(obj.Object["spec"].(map[string]interface{}))
			}
			if err := validateGroupSourceFence(context.Background(), fake.NewClientBuilder().WithObjects(p).Build(), r); err == nil {
				t.Fatal("invalid group spec accepted")
			}
		})
	}
}
