package framework

import (
	"context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func TestInitialNativeRescheduleDoesNotMoveAdmittedExecution(t *testing.T) {
	for _, scenario := range []string{"waiting", "committed", "permit", "no-gate", "no-cloud", "all-scheduled", "no-shortage", "throttled", "policy-not-observed"} {
		t.Run(scenario, func(t *testing.T) {
			run, work := initialObjects("Execute")
			var s RunSpec
			_ = decodeSpec(run, &s)
			s.ExecutionGate = true
			s.Cloud.Cluster = "aws1"
			now := time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC)
			run.Object["status"] = map[string]interface{}{"cloudEligible": true}
			b := object("work.karmada.io/v1alpha2", "ResourceBinding")
			b.SetName("binding")
			b.SetNamespace(run.GetNamespace())
			b.SetUID("binding-uid")
			b.Object["spec"] = map[string]interface{}{"resource": map[string]interface{}{"name": work.GetName(), "uid": string(work.GetUID()), "kind": "StatefulSet"}, "clusters": []interface{}{map[string]interface{}{"name": "onpre1", "replicas": int64(1)}}, "placement": map[string]interface{}{"clusterAffinities": []interface{}{map[string]interface{}{"clusterNames": []interface{}{"onpre1"}}, map[string]interface{}{"clusterNames": []interface{}{"aws1"}}}}}
			o := WorkloadObservation{GPUShortage: true}
			ready := true
			switch scenario {
			case "committed":
				_ = unstructured.SetNestedField(run.Object, "onpre1", "status", "committedCluster")
			case "permit":
				_ = unstructured.SetNestedMap(run.Object, map[string]interface{}{"pods": map[string]interface{}{"p": "uid"}}, "status", "executionPermit")
			case "no-gate":
				s.ExecutionGate = false
			case "no-cloud":
				ready = false
			case "all-scheduled":
				o.AllPodsScheduled = true
			case "no-shortage":
				o.GPUShortage = false
			case "throttled":
				_ = unstructured.SetNestedField(b.Object, stamp(now.Add(-30*time.Second)), "spec", "rescheduleTriggeredAt")
			case "policy-not-observed":
				unstructured.RemoveNestedField(b.Object, "spec", "placement")
			}
			before := b.DeepCopy()
			c := frameworkClient(run, work, b)
			yes, err := requestInitialReschedule(context.Background(), c, run, work, s, o, ready, now)
			if err != nil {
				t.Fatal(err)
			}
			if yes != (scenario == "waiting") {
				t.Fatalf("requested=%v scenario=%s", yes, scenario)
			}
			_ = c.Get(context.Background(), client.ObjectKeyFromObject(b), b)
			if yes {
				unstructured.RemoveNestedField(b.Object, "spec", "rescheduleTriggeredAt")
			}
			if !reflect.DeepEqual(b.Object["spec"], before.Object["spec"]) {
				t.Fatal("changed native binding selection or unrelated fields")
			}
		})
	}
}
