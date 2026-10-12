package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPeriodicDisabledPreventsScheduleAndCanBeReenabled(t *testing.T) {
	now := mustParseTime(t, "2026-10-12T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	policy.SetAnnotations(map[string]string{"training.dcnlab.com/periodic-checkpoint": "disabled"})
	r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeFixture(now), riskFixture(now), workloadFixture("workload-uid"))
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		list := p.NewList("FluidCRMigration")
		if err := r.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Fatal("disabled policy created checkpoint")
		}
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	if got := stringField(policy.Object, "status", "checkpoint", "reason"); got != "periodic_disabled" {
		t.Fatalf("reason=%s", got)
	}
	policy.SetAnnotations(nil)
	if err := r.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	list := p.NewList("FluidCRMigration")
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatal("reenabled policy did not create schedule")
	}
}

func TestPeriodicDisabledPausesExistingSchedule(t *testing.T) {
	now := mustParseTime(t, "2026-10-12T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	policy.SetAnnotations(map[string]string{"training.dcnlab.com/periodic-checkpoint": "disabled"})
	input := p.ReadPolicyInput(policy)
	schedule := checkpointScheduleFixture(input, true)
	r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, schedule)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(schedule), schedule); err != nil {
		t.Fatal(err)
	}
	if boolField(schedule.Object, "spec", "schedule", "enabled") {
		t.Fatal("existing schedule still enabled")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	if got := stringField(policy.Object, "status", "checkpoint", "reason"); got != "waiting_for_schedule_pause" {
		t.Fatalf("reason=%s", got)
	}
}
