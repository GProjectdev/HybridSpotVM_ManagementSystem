package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPeriodicDelayPreventsInitialSchedule(t *testing.T) {
	now := mustParseTime(t, "2026-10-12T00:00:00Z")
	policy := checkpointPolicyFixture(now)
	policy.SetAnnotations(map[string]string{"training.dcnlab.com/periodic-not-before": now.Add(2 * time.Hour).Format(time.RFC3339)})
	checkpoint := policy.Object["spec"].(map[string]interface{})["checkpoint"].(map[string]interface{})
	checkpoint["minIntervalSeconds"] = int64(7200)
	checkpoint["maxIntervalSeconds"] = int64(7200)
	r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeFixture(now), riskFixture(now), workloadFixture("workload-uid"))
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	list := p.NewList("FluidCRMigration")
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatal("deferred policy created a checkpoint")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	if got := stringField(policy.Object, "status", "checkpoint", "reason"); got != "periodic_deferred" {
		t.Fatalf("reason=%s", got)
	}
	policy.SetAnnotations(map[string]string{"training.dcnlab.com/periodic-not-before": now.Format(time.RFC3339)})
	if err := r.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || intField(list.Items[0].Object, "spec", "schedule", "intervalSeconds") != 7200 {
		t.Fatal("expired delay must create the 7200-second schedule")
	}
}

func TestPeriodicDelayBoundaryAndInvalidValue(t *testing.T) {
	now := mustParseTime(t, "2026-10-12T00:00:00Z")
	obj := checkpointPolicyFixture(now)
	for _, tc := range []struct {
		value            string
		delayed, invalid bool
	}{
		{"", false, false},
		{now.Add(2 * time.Hour).Format(time.RFC3339Nano), true, false},
		{now.Format(time.RFC3339Nano), false, false},
		{now.Add(-time.Second).Format(time.RFC3339Nano), false, false},
		{"bad", true, true},
	} {
		obj.SetAnnotations(map[string]string{"training.dcnlab.com/periodic-not-before": tc.value})
		got, err := periodicDeferred(obj, now)
		if got != tc.delayed || (err != nil) != tc.invalid {
			t.Fatalf("value=%q delayed=%v err=%v", tc.value, got, err)
		}
		if tc.delayed {
			r := checkpointReconcilerFixture(t, func() time.Time { return now })
			paused, err := r.scheduleMustPause(context.Background(), obj, p.ReadPolicyInput(obj))
			if err != nil || !paused {
				t.Fatalf("delay must pause existing schedules: %v %v", paused, err)
			}
		}
	}
}
