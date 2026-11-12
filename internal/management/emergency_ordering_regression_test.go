package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEmergencyOrderingCapacityIntentBeforePauseAck(t *testing.T) {
	for _, scenario := range []string{"pending", "stale-ack", "suspended", "stale-workload", "fenced", "group-route"} {
		t.Run(scenario, func(t *testing.T) {
			now := mustParseTime(t, "2026-09-28T00:00:00Z")
			policy, old, objects := fixedEmergencyFixtures(now, false, spotInterruptionNotice)
			switch scenario {
			case "suspended":
				policy.SetAnnotations(map[string]string{"training.dcnlab.com/suspend": "true"})
			case "stale-workload":
				for _, obj := range objects {
					if obj.GetKind() == "StatefulSet" {
						obj.SetUID("changed-workload")
					}
				}
			case "fenced":
				_ = unstructured.SetNestedMap(old.Object, map[string]interface{}{"operation": "another"}, "spec", "fence")
			case "group-route":
				policy.SetAnnotations(map[string]string{"training.dcnlab.com/planned-partial": "disabled"})
			}
			input := p.ReadPolicyInput(policy)
			schedule := checkpointScheduleFixture(input, true)
			schedule.SetGeneration(3)
			if scenario == "stale-ack" {
				_ = unstructured.SetNestedField(schedule.Object, false, "spec", "schedule", "enabled")
				schedule.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": input.SourceCluster, "phase": "Paused", "observedGeneration": int64(2)}}}
			}
			objects = append(objects, schedule)
			r := checkpointReconcilerFixture(t, func() time.Time { return now }, objects...)
			ctx := context.Background()
			for i := 0; i < 2; i++ {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
					t.Fatal(err)
				}
			}
			ops := newSpotReplacementList()
			if err := r.List(ctx, ops); err != nil {
				t.Fatal(err)
			}
			want := 0
			if scenario == "pending" || scenario == "stale-ack" {
				want = 1
			}
			if len(ops.Items) != want {
				_ = r.Get(ctx, client.ObjectKeyFromObject(policy), policy)
				t.Fatalf("operations=%d want=%d status=%#v", len(ops.Items), want, policy.Object["status"])
			}
			if want == 1 {
				op := &ops.Items[0]
				if stringField(op.Object, "spec", "oldNodeProvisionRef", "uid") != string(old.GetUID()) || op.GetAnnotations()[annotationEmergencyEventID] == "" {
					t.Fatal("capacity intent lost source UID or event binding")
				}
				vm := &ReplacementCapacityReconciler{Client: r.Client, APIReader: r.Client, Clock: r.Clock}
				if _, err := vm.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
					t.Fatal(err)
				}
				node := p.NewObject("NodeProvision")
				if err := r.Get(ctx, client.ObjectKey{Namespace: policy.GetNamespace(), Name: stringField(op.Object, "spec", "replacementNodeProvisionRef", "name")}, node); err != nil {
					t.Fatalf("capacity not provisioned while pause pending: %v", err)
				}
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			if boolField(policy.Object, "status", "checkpoint", "periodicQuiesced") {
				t.Fatal("pause acknowledged without fresh member receipt")
			}
			migrations := p.NewList("FluidCRMigration")
			if err := r.List(ctx, migrations); err != nil {
				t.Fatal(err)
			}
			if len(migrations.Items) != 2 {
				t.Fatalf("competing checkpoint created: %d migrations", len(migrations.Items))
			}
		})
	}
}

func TestEmergencyOrderingFallbackBeforeCapacityReady(t *testing.T) {
	for _, scenario := range []string{"missing", "provisioning", "no-uid", "before-deadline", "source-uid-changed"} {
		t.Run(scenario, func(t *testing.T) {
			now := mustParseTime(t, "2026-09-28T00:02:00Z")
			op, policy, runtime, old, replacement, partial, full, sts := emergencyFallbackFixtures(t, now)
			_ = unstructured.SetNestedField(replacement.Object, "Provisioning", "status", "phase")
			if scenario == "no-uid" {
				replacement.SetUID("")
			}
			if scenario == "before-deadline" {
				_ = unstructured.SetNestedField(old.Object, now.Add(time.Minute).Format(time.RFC3339), "status", "spot", "interruptionTime")
			}
			if scenario == "source-uid-changed" {
				old.SetUID("changed")
			}
			objects := []client.Object{op, policy, runtime, old, partial, full, sts, replacementFallbackSurvivorNode()}
			if scenario != "missing" {
				objects = append(objects, replacement)
			}
			r := replacementFallbackReconcilerFixture(t, now, objects...)
			ctx := context.Background()
			// Use the production reconciler, not the fixture's staged helper.
			if _, err := r.ReplacementReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(op), op); err != nil {
				t.Fatal(err)
			}
			fallback, found, _ := unstructured.NestedMap(op.Object, "status", "partialFallback")
			want := scenario != "before-deadline" && scenario != "source-uid-changed"
			if found != want || (want && stringField(fallback, "reason") != emergencyFallbackReason) {
				t.Fatalf("fallback=%#v want=%v status=%#v", fallback, want, op.Object["status"])
			}
			if want {
				cp := &CheckpointReconciler{Client: r.Client, Clock: r.Clock}
				if err := cp.coordinateReplacementCheckpoint(ctx, op); err == nil {
					t.Fatal("recorded fallback did not fence checkpoint coordination")
				}
			}
			for _, suffix := range []string{"-restore", "-group-restore"} {
				if err := r.Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: op.GetName() + suffix}, newRestoreRequest()); !apierrors.IsNotFound(err) {
					t.Fatalf("restore created before capacity readiness: %v", err)
				}
			}
		})
	}
}
