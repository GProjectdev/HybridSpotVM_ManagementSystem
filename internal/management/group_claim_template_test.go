package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"testing"
	"time"
)

func TestGroupCheckpointClaimTemplate(t *testing.T) {
	for _, workers := range []int64{1, 2} {
		_, input := groupCheckpointFixture()
		input.TargetWorkers = workers
		sts := p.NewObject("StatefulSet")
		sts.SetNamespace(input.Namespace)
		sts.SetName(input.WorkloadRef.Name)
		sts.SetUID(input.WorkloadRef.UID)
		sts.Object["spec"] = map[string]interface{}{
			"volumeClaimTemplates": []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "checkpoint"}}},
			"template": map[string]interface{}{"spec": map[string]interface{}{
				"containers": []interface{}{map[string]interface{}{
					"name":         "trainer",
					"env":          []interface{}{map[string]interface{}{"name": "FLUIDCR_CHECKPOINT_DIR", "value": "/checkpoint"}},
					"volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}},
				}},
			}},
		}
		fixture := checkpointReconcilerFixture(t, time.Now, sts)
		r := &PolicyReconciler{Client: fixture.Client, APIReader: fixture.Client}
		claim, root, err := r.groupCheckpointVolume(context.Background(), input)
		if workers == 1 && (err != nil || claim != "checkpoint-"+input.WorkloadRef.Name+"-0" || root != "/checkpoint") {
			t.Fatalf("claim=%s root=%s err=%v", claim, root, err)
		}
		if workers == 2 && err == nil {
			t.Fatal("independent rank claims accepted as shared")
		}
	}
}
