package management

import (
	"context"
	"reflect"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func continuityFixture() (*unstructured.Unstructured, *unstructured.Unstructured) {
	old := replacementOldNodeProvisionFixture()
	setEmergencyReplacementSpotEvidence(old, "original")
	_ = unstructured.SetNestedField(old.Object, "aws", "status", "observedCluster")
	_ = unstructured.SetNestedField(old.Object, "2026-10-19T00:00:00Z", "status", "spot", "noticeTime")
	_ = unstructured.SetNestedField(old.Object, spotRebalanceRecommendation, "status", "spot", "signalType")
	op := replacementOperationFixture()
	op.SetName(replacementOperationName(old.GetName(), string(old.GetUID())))
	_ = unstructured.SetNestedField(op.Object, op.GetName(), "spec", "operation")
	a := emergencyBinding(old)
	a[annotationEmergencyEventID] = "original"
	op.SetAnnotations(a)
	_ = unstructured.SetNestedField(old.Object, "subsequent", "status", "spot", "eventID")
	_ = unstructured.SetNestedField(old.Object, spotInterruptionNotice, "status", "spot", "signalType")
	return op, old
}

func TestEmergencyEventContinuityValidation(t *testing.T) {
	for _, name := range []string{"valid", "uid", "instance", "spot-instance", "legacy", "missing-event", "missing-time", "earlier", "downgrade", "missing-signal", "not-at-risk"} {
		t.Run(name, func(t *testing.T) {
			op, old := continuityFixture()
			a := op.GetAnnotations()
			switch name {
			case "uid":
				old.SetUID("another")
			case "instance":
				_ = unstructured.SetNestedField(old.Object, "other", "status", "instanceId")
				_ = unstructured.SetNestedField(old.Object, "other", "status", "spot", "instanceID")
			case "spot-instance":
				_ = unstructured.SetNestedField(old.Object, "other", "status", "spot", "instanceID")
			case "legacy":
				delete(a, annotationEmergencyInstanceID)
			case "missing-event":
				unstructured.RemoveNestedField(old.Object, "status", "spot", "eventID")
			case "missing-time":
				delete(a, annotationEmergencyNotice)
			case "earlier":
				_ = unstructured.SetNestedField(old.Object, "2026-10-18T00:00:00Z", "status", "spot", "noticeTime")
			case "downgrade":
				a[annotationEmergencySignal] = spotInterruptionNotice
				_ = unstructured.SetNestedField(old.Object, spotRebalanceRecommendation, "status", "spot", "signalType")
			case "missing-signal":
				delete(a, annotationEmergencySignal)
			case "not-at-risk":
				_ = unstructured.SetNestedField(old.Object, false, "status", "spot", "atRisk")
			}
			op.SetAnnotations(a)
			spec, err := readReplacementSpec(op)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyReplacementOldNode(old, spec); (err == nil) != (name == "valid") {
				t.Fatalf("replacement: %v", err)
			}
			// UID checks are owned by the cleanup deletion gate; exercise its RIC validator separately.
			if name != "uid" {
				recovery := recoverySpec{SourceCluster: "aws", TargetCluster: "aws", OldMarketType: "Spot", DesiredMarketType: "OnDemand", ReplacementNodeProvisionName: "new", EventID: "original", EmergencyEventID: "original", EmergencyBinding: a}
				if err := verifyOldNodeRIC(old, recovery, restoreEvidence{SourceNodes: map[string]bool{"old-node": true}}); (err == nil) != (name == "valid") {
					t.Fatalf("cleanup: %v", err)
				}
			}
		})
	}
}

func TestEmergencyContinuationKeepsOperationAndCheckpoint(t *testing.T) {
	ctx := context.Background()
	op, old := continuityFixture()
	policy := replacementPolicyFixture()
	_ = unstructured.SetNestedField(policy.Object, true, "spec", "replacement", "enabled")
	_ = unstructured.SetNestedField(policy.Object, "aws", "spec", "sourceCluster")
	_ = unstructured.SetNestedField(policy.Object, "aws", "spec", "capacity", "aws", "karmadaCluster")
	r := replacementReconcilerFixture(t, mustParseTime(t, "2026-10-19T00:01:00Z"), op, old, policy)
	cp := &CheckpointReconciler{Client: r.Client, Clock: r.Clock}
	input := p.ReadPolicySpec(policy)
	for i := 0; i < 2; i++ {
		name, err := cp.ensureEmergencyReplacement(ctx, policy, input)
		if err != nil || name != op.GetName() {
			t.Fatalf("producer name=%s err=%v", name, err)
		}
		producer := &PolicyReconciler{Client: r.Client}
		selected, created, err := producer.ensurePlannedPartialReplacement(ctx, policy, input, old, op.GetName(), "new", "OnDemand", "subsequent")
		if err != nil || !selected || created {
			t.Fatalf("partial producer: %v %v %v", selected, created, err)
		}
		if phase, err := r.reconcileReplacement(ctx, op); phase != replacementPhaseAwaitingReplacementReady || (i == 0 && err != nil) {
			t.Fatalf("phase=%s err=%v", phase, err)
		}
		if i == 0 {
			node := p.NewObject("NodeProvision")
			if err := r.Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: "new"}, node); err != nil {
				t.Fatal(err)
			}
			node.SetUID("new-uid")
			if err := r.Update(ctx, node); err != nil {
				t.Fatal(err)
			}
		}
		if err := cp.coordinateReplacementCheckpoint(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	stored := newSpotReplacementObject()
	if err := r.Get(ctx, client.ObjectKeyFromObject(op), stored); err != nil {
		t.Fatal(err)
	}
	if stored.GetAnnotations()[annotationEmergencyEventID] != "original" {
		t.Fatal("original event overwritten")
	}
	ops := newSpotReplacementList()
	if err := r.List(ctx, ops); err != nil || len(ops.Items) != 1 {
		t.Fatalf("duplicate operation: %v", err)
	}
	assertMigrationCount(t, r.Client, 1)
	for _, phase := range []string{"Completed", "Rejected"} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(op), op); err != nil {
			t.Fatal(err)
		}
		op.Object["status"] = map[string]interface{}{"phase": phase, "message": "retain rejection detail", "observedGeneration": int64(1)}
		if err := r.Status().Update(ctx, op); err != nil {
			t.Fatal(err)
		}
		before := op.DeepCopy()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(op), op); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before.Object["status"], op.Object["status"]) {
			t.Fatal("terminal status changed")
		}
		if got, err := r.reconcileReplacement(ctx, op); got != phase || err != nil {
			t.Fatalf("terminal reopened: %s %v", got, err)
		}
	}
}

func TestEmergencyCleanupBindingTransport(t *testing.T) {
	ctx := context.Background()
	op, old := continuityFixture()
	spec, err := readReplacementSpec(op)
	if err != nil {
		t.Fatal(err)
	}
	r := replacementReconcilerFixture(t, mustParseTime(t, "2026-10-19T00:01:00Z"), op)
	runtimeObj := object("TrainingRuntime", "default", "runtime", "runtime-uid")
	restore := restoreRequest("restore", "restore-uid", 7, "old-node")
	migration := p.NewObject("FluidCRMigration")
	migration.SetAnnotations(map[string]string{"training.dcnlab.com/checkpoint-id": "original-checkpoint"})
	replacement := replacementReadyNodeProvisionFixture()
	cleanupSpec := replacementRecoverySpec(spec, runtimeObj, restore, migration, old, replacement)
	cleanup, created, err := r.ensureSpotRecovery(ctx, op.GetNamespace(), spec, cleanupSpec)
	if err != nil || !created {
		t.Fatalf("cleanup creation: %v %v", created, err)
	}
	stored := p.NewObject("SpotRecovery")
	if err := r.Get(ctx, client.ObjectKeyFromObject(cleanup), stored); err != nil {
		t.Fatal(err)
	}
	roundtrip := readRecoverySpec(stored)
	if roundtrip.EmergencyEventID != "original" || roundtrip.CheckpointID != "original-checkpoint" {
		t.Fatal("original references changed")
	}
	verifier := &RecoveryReconciler{Client: r.Client}
	if err := verifier.verifySpotReplacement(ctx, op.GetNamespace(), roundtrip); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{annotationEmergencyInstanceID, annotationEmergencySignal, annotationEmergencyNotice} {
		if roundtrip.EmergencyBinding[key] != spec.EmergencyBinding[key] {
			t.Fatalf("binding lost: %s", key)
		}
		mutated := stored.DeepCopy()
		a := mutated.GetAnnotations()
		a[key] = "tampered"
		mutated.SetAnnotations(a)
		if err := verifier.verifySpotReplacement(ctx, op.GetNamespace(), readRecoverySpec(mutated)); err == nil {
			t.Fatalf("accepted changed %s", key)
		}
	}
}
