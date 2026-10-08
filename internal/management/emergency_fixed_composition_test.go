package management

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEmergencyFixedCompositionMarket(t *testing.T) {
	for _, mode := range []string{"partial", "group-healthy", "group-unhealthy"} {
		group := mode != "partial"
		for _, signal := range []string{spotInterruptionNotice, spotRebalanceRecommendation} {
			for _, fixed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/signal=%s/fixed=%v", mode, signal, fixed), func(t *testing.T) {
					now := mustParseTime(t, "2026-10-08T00:00:00Z")
					policy, old, objects := fixedEmergencyFixtures(now, group, signal)
					if mode == "group-healthy" {
						sourceStatusForFixedEmergency(objects[1])["phase"] = "Running"
					}
					market := "OnDemand"
					if fixed {
						market = "Spot"
						_ = unstructured.SetNestedField(policy.Object, int64(1), "spec", "policy", "fixedOnDemand")
					}
					r := checkpointReconcilerFixture(t, func() time.Time { return now }, objects...)
					input := p.ReadPolicySpec(policy)
					name, err := r.ensureEmergencyReplacement(context.Background(), policy, input)
					operation := replacementOperationName(old.GetName(), string(old.GetUID()))
					if fixed && !group {
						if name != "" || err == nil || !strings.Contains(err.Error(), "unsupported without training.dcnlab.com/planned-partial=disabled") {
							t.Fatalf("fixed partial must fail closed: name=%q err=%v", name, err)
						}
						ops := newSpotReplacementList()
						if err := r.List(context.Background(), ops); err != nil {
							t.Fatal(err)
						}
						if len(ops.Items) != 0 {
							t.Fatal("unsupported partial created operation")
						}
						nodes := p.NewList("NodeProvision")
						if err := r.List(context.Background(), nodes); err != nil {
							t.Fatal(err)
						}
						if len(nodes.Items) != 2 {
							t.Fatal("unsupported partial provisioned a replacement")
						}
						return
					}
					if group {
						wantName := operation
						if !fixed && mode == "group-healthy" {
							wantName = ""
						}
						if name != wantName || err == nil || !strings.Contains(err.Error(), "waiting for group replacement NodeProvision") {
							t.Fatalf("group name=%q err=%v", name, err)
						}
						replacement := p.NewObject("NodeProvision")
						key := client.ObjectKey{Namespace: input.Namespace, Name: replacementNodeProvisionName(old.GetName(), string(old.GetUID()))}
						if err := r.Get(context.Background(), key, replacement); err != nil {
							t.Fatal(err)
						}
						if got := stringField(replacement.Object, "spec", "marketType"); got != market {
							t.Fatalf("group replacement market=%q, want %q", got, market)
						}
						if replacement.GetAnnotations()[groupOldUID] != string(old.GetUID()) {
							t.Fatal("lost source UID")
						}
						// Simulate the provisioner becoming ready, then verify the restore retains event provenance.
						replacement.SetUID("replacement-uid")
						if err := r.Update(context.Background(), replacement); err != nil {
							t.Fatal(err)
						}
						replacement.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": "replacement-node"}
						if err := r.Update(context.Background(), replacement); err != nil {
							t.Fatal(err)
						}
						if _, err := r.ensureEmergencyReplacement(context.Background(), policy, input); err != nil {
							t.Fatal(err)
						}
						req := newRestoreRequest()
						if err := r.Get(context.Background(), client.ObjectKey{Namespace: input.Namespace, Name: operation + "-group-restore"}, req); err != nil {
							t.Fatal(err)
						}
						if req.GetAnnotations()[annotationEmergencyEventID] != "notice" || req.GetAnnotations()[groupNewUID] != "replacement-uid" {
							t.Fatalf("lost emergency provenance: %v", req.GetAnnotations())
						}
					} else {
						if err != nil || name != operation {
							t.Fatalf("partial name=%q err=%v", name, err)
						}
						op := newSpotReplacementObject()
						if err := r.Get(context.Background(), client.ObjectKey{Namespace: input.Namespace, Name: name}, op); err != nil {
							t.Fatal(err)
						}
						if stringField(op.Object, "spec", "desiredMarketType") != market || op.GetAnnotations()[annotationEmergencyEventID] != "notice" {
							t.Fatalf("partial market/provenance: %v", op.Object)
						}
					}
					if _, err := r.ensureEmergencyReplacement(context.Background(), policy, input); err != nil {
						t.Fatal(err)
					}
					ops := newSpotReplacementList()
					if err := r.List(context.Background(), ops); err != nil {
						t.Fatal(err)
					}
					want := 1
					if group {
						want = 0
					}
					if len(ops.Items) != want {
						t.Fatalf("partial operations=%d, want %d", len(ops.Items), want)
					}
				})
			}
		}
	}
}

func TestEmergencyFixedCompositionInvalidPolicyStatus(t *testing.T) {
	for _, count := range []int64{-1, 0, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			now := mustParseTime(t, "2026-10-08T00:00:00Z")
			policy, _, objects := fixedEmergencyFixtures(now, false, spotInterruptionNotice)
			_ = unstructured.SetNestedField(policy.Object, int64(1), "spec", "policy", "minOnDemand")
			_ = unstructured.SetNestedField(policy.Object, count, "spec", "policy", "fixedOnDemand")
			r := checkpointReconcilerFixture(t, func() time.Time { return now }, objects...)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
			manager := &PolicyReconciler{Client: r.Client, APIReader: r.Client, Clock: r.Clock}
			if _, err := manager.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			got := p.NewObject("TrainingPolicy")
			if err := r.Get(context.Background(), request.NamespacedName, got); err != nil {
				t.Fatal(err)
			}
			for _, subtree := range []string{"policy", "checkpoint"} {
				if stringField(got.Object, "status", subtree, "reason") != "invalid_spec" ||
					!strings.Contains(stringField(got.Object, "status", subtree, "message"), "fixedOnDemand") {
					t.Fatalf("%s did not reject invalid bounds: %v", subtree, got.Object["status"])
				}
			}
			if !boolField(got.Object, "status", "policy", "provisioningBlocked") {
				t.Fatal("invalid bounds did not block provisioning")
			}
			ops := newSpotReplacementList()
			if err := r.List(context.Background(), ops); err != nil {
				t.Fatal(err)
			}
			if len(ops.Items) != 0 {
				t.Fatal("invalid bounds created replacement")
			}
		})
	}
}

func TestEmergencyFixedCompositionRejectsInvalidBounds(t *testing.T) {
	for _, count := range []int64{-1, 0, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			now := mustParseTime(t, "2026-10-08T00:00:00Z")
			policy, _, objects := fixedEmergencyFixtures(now, false, spotInterruptionNotice)
			_ = unstructured.SetNestedField(policy.Object, int64(1), "spec", "policy", "minOnDemand")
			_ = unstructured.SetNestedField(policy.Object, count, "spec", "policy", "fixedOnDemand")
			r := checkpointReconcilerFixture(t, func() time.Time { return now }, objects...)
			name, err := r.ensureEmergencyReplacement(context.Background(), policy, p.ReadPolicySpec(policy))
			if name != "" || err == nil || !strings.Contains(err.Error(), "fixedOnDemand") {
				t.Fatalf("name=%q err=%v", name, err)
			}
			ops := newSpotReplacementList()
			if err := r.List(context.Background(), ops); err != nil {
				t.Fatal(err)
			}
			if len(ops.Items) != 0 {
				t.Fatal("invalid policy created operation")
			}
		})
	}
}

func fixedEmergencyFixtures(now time.Time, group bool, signal string) (*unstructured.Unstructured, *unstructured.Unstructured, []*unstructured.Unstructured) {
	policy, runtime, old := emergencyReplacementFixtures(now)
	runtime.SetUID("runtime-uid")
	report := sourceStatusForFixedEmergency(runtime)
	report["sourceWorldUID"] = "workload-uid"
	report["sourcePods"] = report["pods"]
	_ = unstructured.SetNestedField(old.Object, signal, "status", "spot", "signalType")
	_ = unstructured.SetNestedField(old.Object, "Ready", "status", "phase")
	_ = unstructured.SetNestedField(old.Object, "member-1", "status", "memberUID")
	survivor := old.DeepCopy()
	survivor.SetName("node-0")
	survivor.SetUID("survivor-uid")
	_ = unstructured.SetNestedField(survivor.Object, "OnDemand", "spec", "marketType")
	survivor.Object["status"] = map[string]interface{}{"nodeName": "node-0", "memberUID": "member-0", "instanceId": "i-survivor", "phase": "Ready"}
	workload := workloadFixture("workload-uid")
	workload.Object["spec"] = map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{
		"containers": []interface{}{map[string]interface{}{"name": "trainer", "volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}}}},
		"volumes":    []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}}},
	}}}
	cp, _ := groupCheckpointFixture()
	cp.SetLabels(map[string]string{p.LabelPolicyUID: string(policy.GetUID())})
	_ = unstructured.SetNestedField(cp.Object, "workload-uid", "spec", "workloadRef", "uid")
	objects := []*unstructured.Unstructured{policy, runtime, old, survivor, workload, cp}
	for _, obj := range objects {
		obj.SetNamespace("demo")
	}
	if group {
		report["phase"] = "Unavailable"
		policy.SetAnnotations(map[string]string{"training.dcnlab.com/planned-partial": "disabled", groupIntentAnnotation: string(old.GetUID())})
		_ = unstructured.SetNestedMap(policy.Object, map[string]interface{}{"periodicQuiesced": true, "replacementOperation": string(old.GetUID()), "reason": "group_intent_quiesced"}, "status", "checkpoint")
	}
	return policy, old, objects
}

func sourceStatusForFixedEmergency(runtime *unstructured.Unstructured) map[string]interface{} {
	return runtime.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})["status"].(map[string]interface{})
}
