package framework

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func transitionFixture() (*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, RunSpec, TransitionSpec, time.Time) {
	run, work := initialObjects("Execute")
	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	rs := RunSpec{WorkloadRef: ObjectRef{Name: "a", UID: "source"}, OnPremiseCluster: "onpre1", WorldSize: 1, CheckpointSeconds: 60, Mode: "Execute", ExecutionGate: true, Cloud: CloudSpec{Allowed: true, Cluster: "aws1", TemplateRef: "gpu", RiskProfileRef: "risk", MaxWorkers: 2, Deadline: stamp(now.Add(time.Hour))}}
	run.Object["spec"] = encode(rs)
	run.Object["status"] = map[string]interface{}{"workloadUID": "source", "committedCluster": "aws1", "executionPermit": map[string]interface{}{"workloadUID": "source", "cluster": "aws1", "memberWorkloadUID": "member-work", "pods": map[string]interface{}{"a-0": "old-pod"}, "nodes": map[string]interface{}{"a-0": "old-node"}, "issuedAt": stamp(now.Add(-time.Hour))}}
	cap := named("TrainingCapacity", "ns", initialCapacityName(run))
	cap.SetUID("cap")
	cap.SetGeneration(1)
	setOwner(cap, run)
	cap.Object["spec"] = encode(CapacitySpec{RunRef: ObjectRef{Name: "a", UID: "run"}, Cluster: "aws1", TemplateRef: "gpu", MaxWorkers: 2, Deadline: rs.Cloud.Deadline, Allocations: []Allocation{{Name: "old", State: "Retain", Market: "spot"}}})
	cap.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "observedAt": stamp(now), "allocations": map[string]interface{}{"old": map[string]interface{}{"phase": "Ready", "nodeName": "old-node", "nodeUID": "old-node-uid", "memberUID": "old-member", "instanceID": "i-old"}}}
	ts := TransitionSpec{WorkloadTemplateSHA256: workloadTemplateSHA(work), RunRef: ObjectRef{Name: "a", UID: "run"}, Mode: "PlannedReplacement", WorkloadUID: "source", ExecutionID: executionID(run), WorldSize: 1, SourceCluster: "aws1", TargetCluster: "aws1", CapacityRef: ObjectRef{Name: cap.GetName(), UID: "cap"}, SourcePods: []SourcePod{{Rank: 0, PodName: "a-0", PodUID: "old-pod", NodeName: "old-node", NodeProvisionRef: &ProviderRef{Name: "old", UID: "old-member", InstanceID: "i-old"}}}, TargetAllocations: []string{"new"}, AddedAllocations: []Allocation{{Name: "new", State: "Prepare", Market: "on-demand"}}, RetireAllocations: []string{"old"}, TriggerID: "risk"}
	tr := named("TrainingTransition", "ns", "t")
	tr.SetUID("transition")
	tr.SetGeneration(1)
	setOwner(tr, run)
	tr.Object["spec"] = encode(ts)
	return run, work, cap, tr, rs, ts, now
}
func TestTransitionPreparesAdditivelyWithoutRetiringOrGranting(t *testing.T) {
	run, work, cap, tr, rs, _, now := transitionFixture()
	c := frameworkClient(run, work, cap, tr)
	r := &RunReconciler{Client: c, Now: func() time.Time { return now }}
	status, _, _ := unstructured.NestedMap(run.Object, "status")
	phase, _, e := r.reconcileTransitionIntent(context.Background(), run, work, tr, rs, status)
	if e != nil || phase != "PreparingTransitionCapacity" {
		t.Fatal(phase, e)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cap), cap)
	var cs CapacitySpec
	_ = decodeSpec(cap, &cs)
	if len(cs.Allocations) != 2 || cs.Allocations[0].State != "Retain" || cs.Allocations[1].Market != "on-demand" {
		t.Fatal(cs)
	}
	if status["executionPermit"].(map[string]interface{})["pods"].(map[string]interface{})["a-0"] != "old-pod" {
		t.Fatal("permit changed before restore")
	}
	phase, _, e = r.reconcileTransitionIntent(context.Background(), run, work, tr, rs, status)
	if e != nil || phase != "TransitionPreparing" {
		t.Fatal(phase, e)
	}
}
func TestTransitionCannotCheckpointBeforePreparedTarget(t *testing.T) {
	run, work, cap, tr, _, _, now := transitionFixture()
	c := frameworkClient(run, work, cap, tr)
	r := &TransitionReconciler{Client: c, Now: func() time.Time { return now }, Members: func(string) (client.Reader, error) { t.Fatal("target is not Ready"); return nil, nil }}
	if _, e := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tr)}); e != nil {
		t.Fatal(e)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(tr), tr)
	if str(tr, "status", "phase") != "PreparingTarget" {
		t.Fatal(tr.Object["status"])
	}
	for _, kind := range []string{"FluidCRMigration"} {
		items, e := listItems(context.Background(), c, "fluidcr.dcnlab.com/v1alpha1", kind, "ns")
		if e != nil || len(items) != 0 {
			t.Fatal(items, e)
		}
	}
}
func TestFenceOnlyPreservesProviderResourceUntilVerifiedRetirement(t *testing.T) {
	ctx := context.Background()
	r, c, m, cap, member := readyCapacityFixture(t)
	run := named("TrainingRun", "ns", "a")
	_ = c.Get(ctx, key("ns", "a"), run)
	tr := named("TrainingTransition", "ns", "t")
	tr.SetUID("transition")
	setOwner(tr, run)
	tr.Object["spec"] = encode(TransitionSpec{Mode: "Preemption", CapacityRef: ObjectRef{Name: cap.GetName(), UID: string(cap.GetUID())}, RetireAllocations: []string{"worker"}, SourcePods: []SourcePod{{NodeProvisionRef: &ProviderRef{Name: "worker", UID: "member", InstanceID: "i-owned"}}}})
	_ = c.Create(ctx, tr)
	_ = unstructured.SetNestedSlice(cap.Object, []interface{}{encode(Allocation{Name: "worker", Market: "spot", State: "Fence", FenceOperationUID: "transition"})}, "spec", "allocations")
	_ = c.Update(ctx, cap)
	reconcileCapacity(t, r, 1)
	member.SetGeneration(2)
	_ = unstructured.SetNestedMap(member.Object, map[string]interface{}{"phase": "Fenced", "operationUID": "transition", "instanceID": "i-owned", "observedGeneration": int64(2), "observedAt": stamp(r.now())}, "status", "fence")
	_ = m.Update(ctx, member)
	reconcileCapacity(t, r, 3)
	origin := p.NewObject("NodeProvision")
	if e := c.Get(ctx, key("ns", "worker"), origin); e != nil {
		t.Fatal("fence-only deleted evidence", e)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(cap), cap)
	if str(cap, "status", "allocations", "worker", "phase") != "Fenced" {
		t.Fatal(cap.Object["status"])
	}
	_ = unstructured.SetNestedSlice(cap.Object, []interface{}{encode(Allocation{Name: "worker", Market: "spot", State: "Retire", FenceOperationUID: "transition"})}, "spec", "allocations")
	_ = c.Update(ctx, cap)
	reconcileCapacity(t, r, 1)
	if e := c.Get(ctx, key("ns", "worker"), origin); e == nil {
		t.Fatal("explicit Retire not processed")
	}
}
func TestWrongFenceOperationCannotTerminateSource(t *testing.T) {
	run, _, cap, tr, _, ts, _ := transitionFixture()
	ts.Mode = "Preemption"
	tr.Object["spec"] = encode(ts)
	c := frameworkClient(run, cap, tr)
	for _, bad := range []string{"member", "instance", "operation"} {
		entry := map[string]interface{}{"memberUID": "old-member"}
		instance := "i-old"
		a := Allocation{Name: "old", State: "Fence", FenceOperationUID: string(tr.GetUID())}
		switch bad {
		case "member":
			entry["memberUID"] = "recreated"
		case "instance":
			instance = "i-foreign"
		case "operation":
			a.FenceOperationUID = "foreign"
		}
		if _, e := authorizedFenceOperation(context.Background(), c, cap, run, a, entry, instance); e == nil {
			t.Fatal("accepted", bad)
		}
	}
}
func TestSourcePermitSnapshotDoesNotChangeWhenNodeInventoryBackfilled(t *testing.T) {
	run, _, _, _, _, _, _ := transitionFixture()
	a := executionID(run)
	_ = unstructured.SetNestedStringMap(run.Object, map[string]string{"a-0": "other"}, "status", "executionPermit", "nodes")
	if executionID(run) != a {
		t.Fatal("inventory changed execution identity")
	}
	_ = unstructured.SetNestedStringMap(run.Object, map[string]string{"a-0": "new-pod"}, "status", "executionPermit", "pods")
	if executionID(run) == a {
		t.Fatal("new Pod reused identity")
	}
}

func TestRestorePhaseAloneCannotGrantExecutionOrRetire(t *testing.T) {
	for _, phase := range []string{"Pending", "Failed", "Prepared", "Verified"} {
		t.Run(phase, func(t *testing.T) {
			run, work, cap, tr, rs, _, now := transitionFixture()
			var cs CapacitySpec
			_ = decodeSpec(cap, &cs)
			cs.Allocations = append(cs.Allocations, Allocation{Name: "new", State: "Prepare", Market: "on-demand"})
			cap.Object["spec"] = encode(cs)
			req := object("migration.dcnlab.com/v1alpha1", "RestoreRequest")
			req.SetNamespace("ns")
			req.SetName("t-restore")
			req.SetUID("rr")
			req.SetGeneration(1)
			setOwner(req, tr)
			req.Object["spec"] = map[string]interface{}{"groupRestore": map[string]interface{}{"operationUID": "transition"}}
			req.Object["status"] = map[string]interface{}{"phase": phase, "observedGeneration": int64(1)}
			c := frameworkClient(run, work, cap, tr, req)
			r := &RunReconciler{Client: c, Now: func() time.Time { return now }}
			status, _, _ := unstructured.NestedMap(run.Object, "status")
			if _, _, e := r.reconcileTransitionIntent(context.Background(), run, work, tr, rs, status); e != nil {
				t.Fatal(e)
			}
			if status["executionPermit"].(map[string]interface{})["pods"].(map[string]interface{})["a-0"] != "old-pod" {
				t.Fatal("phase alone granted target")
			}
			_ = c.Get(context.Background(), client.ObjectKeyFromObject(cap), cap)
			_ = decodeSpec(cap, &cs)
			if cs.Allocations[0].State != "Retain" {
				t.Fatal("phase alone retired source")
			}
		})
	}
}

func TestCheckpointCreationTimestampResolution(t *testing.T) {
	ready, _ := time.Parse(time.RFC3339Nano, "2026-11-26T08:46:38.180946575Z")
	for _, tc := range []struct {
		at   string
		want bool
	}{
		{"2026-11-26T08:46:37Z", false},
		{"2026-11-26T08:46:38Z", true},
		{"2026-11-26T08:46:39Z", true},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		if checkpointCreatedAfterPreparation(at, ready) != tc.want {
			t.Fatal(tc)
		}
	}
	if checkpointCreatedAfterPreparation(time.Time{}, ready) || checkpointCreatedAfterPreparation(ready, time.Time{}) {
		t.Fatal("missing preparation evidence accepted")
	}
}
