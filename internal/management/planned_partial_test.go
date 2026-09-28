package management

import (
	"context"
	"fmt"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func TestPlannedPartialRouting(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		rank                               int
		stale, changed, risk, group        bool
		selected, reject                   bool
		emergency, wrongInstance, notReady bool
	}{
		{name: "healthy nonzero rank", rank: 1, selected: true},
		{name: "healthy rank zero", rank: 0, selected: true},
		{name: "stale health uses group", rank: 1, stale: true},
		{name: "changed UID refuses", rank: 1, changed: true, selected: true, reject: true},
		{name: "at risk uses group", rank: 1, risk: true},
		{name: "group already owns world", rank: 1, group: true},
		{name: "live interruption rank zero", rank: 0, risk: true, emergency: true, selected: true},
		{name: "live interruption rank one", rank: 1, risk: true, emergency: true, selected: true},
		{name: "notice mismatched instance refuses", rank: 1, risk: true, emergency: true, wrongInstance: true, selected: true, reject: true},
		{name: "unavailable node uses group", rank: 1, notReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, input := groupCheckpointFixture()
			input.PolicyName = "policy"
			input.Generation = 1
			input.RuntimeRefName = "runtime"
			input.Capacity.AWSCluster = "aws"
			now := time.Now().UTC().Truncate(time.Second)
			policy := p.NewObject("TrainingPolicy")
			policy.SetName("policy")
			policy.SetNamespace("demo")
			policy.SetUID(input.PolicyUID)
			if tc.group {
				policy.SetAnnotations(map[string]string{groupIntentAnnotation: "group"})
			}
			rt := p.NewObject("TrainingRuntime")
			rt.SetName("runtime")
			rt.SetNamespace("demo")
			rt.SetUID("runtime-uid")
			rt.SetGeneration(1)
			rt.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload"}}
			snapshots, pods := []interface{}{}, []interface{}{}
			objects := []*unstructured.Unstructured{policy, rt}
			var old *unstructured.Unstructured
			for rank := 0; rank < 2; rank++ {
				name := fmt.Sprintf("trainer-%d", rank)
				node := name + "-node"
				snapshots = append(snapshots, map[string]interface{}{"name": name, "uid": name + "-uid", "nodeName": node, "rank": int64(rank)})
				uid := name + "-uid"
				if tc.changed && rank == 1 {
					uid = "changed"
				}
				pods = append(pods, map[string]interface{}{"name": name, "uid": uid, "rank": int64(rank), "observedAt": now.Format(time.RFC3339)})
				np := p.NewObject("NodeProvision")
				np.SetName(fmt.Sprintf("np-%d", rank))
				np.SetNamespace("demo")
				np.SetUID(types.UID(fmt.Sprintf("np-origin-%d", rank)))
				np.SetLabels(map[string]string{p.LabelPolicyUID: "policy"})
				np.Object["spec"] = map[string]interface{}{"marketType": "Spot"}
				np.Object["status"] = map[string]interface{}{"nodeName": node, "instanceId": fmt.Sprintf("i-%d", rank), "memberUID": fmt.Sprintf("member-%d", rank), "phase": "Ready"}
				objects = append(objects, np)
				if rank == tc.rank {
					old = np
				}
			}
			if tc.risk {
				_ = unstructured.SetNestedField(old.Object, true, "status", "spot", "atRisk")
			}
			operation, eventID := "operation", ""
			if tc.emergency {
				eventID = "notice-1"
				operation = replacementOperationName(old.GetName(), string(old.GetUID()))
				instance := stringField(old.Object, "status", "instanceId")
				if tc.wrongInstance {
					instance = "i-stale"
				}
				_ = unstructured.SetNestedMap(old.Object, map[string]interface{}{"atRisk": true, "eventID": eventID, "instanceID": instance, "signalType": spotInterruptionNotice}, "status", "spot")
			}
			if tc.notReady {
				_ = unstructured.SetNestedField(old.Object, "Failed", "status", "phase")
			}
			observed := now
			if tc.stale {
				observed = now.Add(-3 * time.Minute)
			}
			rt.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "status": map[string]interface{}{"observedGeneration": int64(1), "sourceWorldUID": "workload", "sourcePods": snapshots, "phase": "Running", "workloadUID": "workload", "memberWorkloadUID": "member-world", "readyRanks": int64(2), "worldSize": int64(2), "observedAt": observed.Format(time.RFC3339), "pods": pods}}}}
			sts := p.NewObject("StatefulSet")
			sts.SetName("trainer")
			sts.SetNamespace("demo")
			sts.SetUID("workload")
			sts.Object["spec"] = map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "trainer", "volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}}}}, "volumes": []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}}}}}}
			objects = append(objects, sts)
			f := checkpointReconcilerFixture(t, func() time.Time { return now }, objects...)
			r := &PolicyReconciler{Client: f.Client, APIReader: f.Client, Clock: func() time.Time { return now }}
			selected, created, err := r.ensurePlannedPartialReplacement(context.Background(), policy, input, old, operation, "replacement", "OnDemand", eventID)
			if selected != tc.selected || (err != nil) != tc.reject || created != (tc.selected && !tc.reject) {
				t.Fatalf("selected=%v created=%v err=%v", selected, created, err)
			}
			if created {
				createdOp := newSpotReplacementObject()
				createdOp.SetNamespace("demo")
				createdOp.SetName(operation)
				if err := r.Delete(context.Background(), createdOp); err != nil {
					t.Fatal(err)
				}
				// Exercise the public automatic route with no opt-in annotation.
				automaticCreated, automaticErr := r.ensureAutomaticSpotReplacement(context.Background(), policy, input, old, operation, "replacement", "OnDemand", eventID)
				if !automaticCreated || automaticErr != nil {
					t.Fatalf("automatic route lost partial ownership: created=%v err=%v", automaticCreated, automaticErr)
				}
				op := newSpotReplacementObject()
				if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: operation}, op); err != nil {
					t.Fatal(err)
				}
				spec, err := readReplacementSpec(op)
				if err != nil || len(spec.TargetRanks) != 1 || spec.TargetRanks[0] != int64(tc.rank) || len(spec.PreservedSurvivors) != 1 {
					t.Fatalf("spec=%+v err=%v", spec, err)
				}
				if spec.EmergencyEventID != eventID {
					t.Fatalf("notice identity lost: %+v", spec)
				}
				selected, created, err = r.ensurePlannedPartialReplacement(context.Background(), policy, input, old, operation, "replacement", "OnDemand", eventID)
				if !selected || created || err != nil {
					t.Fatalf("idempotency selected=%v created=%v err=%v", selected, created, err)
				}
			}
		})
	}
}
