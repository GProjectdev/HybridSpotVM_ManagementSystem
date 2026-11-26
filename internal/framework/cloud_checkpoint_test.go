package framework

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func TestCloudCheckpointUsesActualSpotAndNeverDisablesOnMissingRisk(t *testing.T) {
	for _, scenario := range []string{"high-risk", "stale-risk", "no-spot", "wrong-capacity", "user-cap"} {
		t.Run(scenario, func(t *testing.T) {
			run, s, cm, risk, now := cloudPolicyFixture()
			_, work := initialObjects("Execute")
			cap := named("TrainingCapacity", run.GetNamespace(), initialCapacityName(run))
			cap.SetUID("cap-uid")
			setOwner(cap, run)
			cap.Object["spec"] = encode(CapacitySpec{RunRef: ObjectRef{Name: run.GetName(), UID: string(run.GetUID())}, Allocations: []Allocation{{Name: "worker", Market: "spot", State: "Retain"}}})
			_ = unstructured.SetNestedField(run.Object, "cap-uid", "status", "initialCapacityUID")
			_ = unstructured.SetNestedField(risk.Object, 1.0, "status", "lambdaPerHour")
			switch scenario {
			case "stale-risk":
				_ = unstructured.SetNestedField(risk.Object, stamp(now.Add(-time.Minute)), "status", "validUntil")
			case "no-spot":
				cap.Object["spec"] = encode(CapacitySpec{RunRef: ObjectRef{Name: run.GetName(), UID: string(run.GetUID())}, Allocations: []Allocation{{Name: "worker", Market: "on-demand", State: "Retain"}}})
			case "wrong-capacity":
				cap.SetUID("foreign")
			case "user-cap":
				s.CheckpointSeconds = 15
			}
			c := frameworkClient(run, work, cm, risk, cap)
			r := &FrameworkCheckpointReconciler{Client: c, Members: func(string) (client.Reader, error) { return c, nil }}
			interval, evidence, err := r.cloudInterval(context.Background(), run, work, p.NewObject("FluidCRMigration"), s, WorkloadObservation{SelectedCluster: "aws1"}, now)
			if scenario == "wrong-capacity" {
				if err == nil {
					t.Fatal("foreign capacity accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if interval <= 0 || interval > s.CheckpointSeconds || interval > 3600 {
				t.Fatal("unsafe interval", interval, evidence)
			}
			if scenario == "no-spot" {
				if evidence["actualSpotWorkers"] != int64(0) || interval != s.CheckpointSeconds {
					t.Fatal(evidence, interval)
				}
				return
			}
			if evidence["actualSpotWorkers"] != int64(1) {
				t.Fatal("desired all-OD policy erased the active Spot risk", evidence)
			}
			if scenario == "stale-risk" && evidence["intervalSource"] != "risk-unavailable-conservative-fallback" {
				t.Fatal(evidence)
			}
		})
	}
}
