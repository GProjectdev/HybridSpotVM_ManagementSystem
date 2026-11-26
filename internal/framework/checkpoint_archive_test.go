package framework

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"strings"
	"testing"
	"time"
)

func TestArchiveImportsOnlyVerifiedCompleteMemberEvidence(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-child", "wrong-parent", "failed", "missing-export", "stale-generation", "future-export"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			run, work := initialObjects("Execute")
			schedule := p.NewObject("FluidCRMigration")
			schedule.SetNamespace("ns")
			schedule.SetName("a-periodic")
			schedule.SetUID("schedule")
			setOwner(schedule, run)
			parent := schedule.DeepCopy()
			parent.SetUID("member-parent")
			parent.SetAnnotations(map[string]string{"resourcetemplate.karmada.io/uid": "schedule"})
			parent.SetLabels(map[string]string{RunUIDLabel: string(run.GetUID())})
			parent.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": string(work.GetUID())}}
			parent.Object["status"] = map[string]interface{}{"lastSuccessfulCheckpoint": map[string]interface{}{"name": "child", "uid": "child-uid"}}
			child := p.NewObject("FluidCRMigration")
			child.SetNamespace("ns")
			child.SetName("child")
			child.SetUID("child-uid")
			child.SetGeneration(1)
			child.SetAnnotations(map[string]string{"training.dcnlab.com/scheduled-parent-uid": "member-parent", "training.dcnlab.com/checkpoint-id": "round-1"})
			child.Object["spec"] = map[string]interface{}{"resume": true, "container": "trainer", "ctrlPort": int64(8298), "workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "a", "uid": string(work.GetUID())}}
			sha := strings.Repeat("a", 64)
			file := map[string]interface{}{"sha256": sha, "filePath": "/var/lib/kubelet/checkpoints/test.tar", "containerName": "trainer", "durableRef": "file-store:ns/sha256/" + sha, "exportedAt": stamp(now.Add(-time.Second)), "checkpointID": "round-1"}
			child.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "phase": "Completed", "completionTime": stamp(now.Add(-2 * time.Second)), "pods": []interface{}{map[string]interface{}{"podName": "a-0", "podUID": "old-pod", "nodeName": "node", "phase": "Resumed", "rank": int64(0), "checkpointID": "round-1", "checkpointFiles": []interface{}{file}}}}
			switch scenario {
			case "wrong-child":
				child.SetUID("other")
			case "wrong-parent":
				parent.SetAnnotations(map[string]string{"resourcetemplate.karmada.io/uid": "other"})
			case "failed":
				_ = unstructured.SetNestedField(child.Object, "Failed", "status", "phase")
			case "missing-export":
				delete(file, "exportedAt")
			case "stale-generation":
				child.SetGeneration(2)
			case "future-export":
				file["exportedAt"] = stamp(now.Add(time.Minute))
			}
			member := frameworkClient(parent, child)
			control := fake.NewClientBuilder().WithObjects(run, work, schedule).WithStatusSubresource(p.NewObject("FluidCRMigration")).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				o.SetUID("archive-uid")
				o.SetGeneration(1)
				return c.Create(ctx, o, opts...)
			}}).Build()
			record, err := archiveCheckpoint(ctx, control, member, run, work, schedule, "onpre1", 1, now)
			if scenario != "valid" {
				if err == nil {
					t.Fatal("invalid archive imported", record)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if record.GetUID() == child.GetUID() || record.GetName() != "archive-child-uid" || record.GetAnnotations()[archiveOrigin] == "" {
				t.Fatal("origin/copy identity not explicit")
			}
			// Repeated import must neither create another command nor alter evidence.
			again, err := archiveCheckpoint(ctx, control, member, run, work, schedule, "onpre1", 1, now)
			if err != nil || again.GetUID() != record.GetUID() {
				t.Fatal(err)
			}
			list := p.NewList("FluidCRMigration")
			if err = control.List(ctx, list, client.InNamespace("ns")); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 2 {
				t.Fatal("duplicate checkpoint command", len(list.Items))
			}
		})
	}
}
