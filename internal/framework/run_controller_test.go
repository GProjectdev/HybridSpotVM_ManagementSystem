package framework

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func frameworkClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	for _, pair := range [][2]string{{"training.dcnlab.com/v1alpha1", "TrainingRun"}, {"training.dcnlab.com/v1alpha1", "TrainingRuntime"}, {"training.dcnlab.com/v1alpha1", "NodeRequest"}, {"training.dcnlab.com/v1alpha1", "TrainingTransition"}, {"policy.karmada.io/v1alpha1", "PropagationPolicy"}, {"work.karmada.io/v1alpha2", "ResourceBinding"}, {"fluidcr.dcnlab.com/v1alpha1", "FluidCRMigration"}, {"apps/v1", "StatefulSet"}} {
		o := object(pair[0], pair[1])
		s.AddKnownTypeWithName(o.GroupVersionKind(), o)
		s.AddKnownTypeWithName(o.GroupVersionKind().GroupVersion().WithKind(pair[1]+"List"), objects(pair[0], pair[1]))
	}
	return fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(named("TrainingRun", "", ""), named("TrainingRuntime", "", "")).WithObjects(objs...).Build()
}
func initialObjects(mode string) (*unstructured.Unstructured, *unstructured.Unstructured) {
	r := named("TrainingRun", "ns", "a")
	r.SetUID("run")
	r.SetGeneration(1)
	r.Object["spec"] = encode(RunSpec{WorkloadRef: ObjectRef{Name: "a"}, OnPremiseCluster: "onpre1", WorldSize: 1, CheckpointSeconds: 600, Mode: mode})
	w := object("apps/v1", "StatefulSet")
	w.SetNamespace("ns")
	w.SetName("a")
	w.SetUID("source")
	w.Object["spec"] = map[string]interface{}{"replicas": int64(1)}
	return r, w
}
func TestRunWritesOnlyOwnedOnPremPlacement(t *testing.T) {
	ctx := context.Background()
	run, work := initialObjects("Execute")
	c := frameworkClient(run, work)
	r := &RunReconciler{Client: c, Members: func(string) (client.Reader, error) { t.Fatal("no member assigned"); return nil, nil }}
	req := ctrl.Request{NamespacedName: key("ns", "a")}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	pp := object("policy.karmada.io/v1alpha1", "PropagationPolicy")
	if err := c.Get(ctx, key("ns", "a-training-placement"), pp); err != nil {
		t.Fatal(err)
	}
	if !owned(pp, run) {
		t.Fatal("placement owner missing")
	}
	groups, _, _ := unstructured.NestedSlice(pp.Object, "spec", "placement", "clusterAffinities")
	if len(groups) != 1 || groups[0].(map[string]interface{})["clusterNames"].([]interface{})[0] != "onpre1" {
		t.Fatal("unexpected placement")
	}
	if err := c.Get(ctx, req.NamespacedName, run); err != nil {
		t.Fatal(err)
	}
	if str(run, "status", "phase") != "WaitingOnPrem" || str(run, "status", "workloadUID") != "source" {
		t.Fatalf("bad status %v", run.Object["status"])
	}
	// Recreation is not silently adopted after a controller restart.
	if err := c.Delete(ctx, work); err != nil {
		t.Fatal(err)
	}
	work.SetResourceVersion("")
	work.SetUID("new")
	if err := c.Create(ctx, work); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, req.NamespacedName, run)
	if str(run, "status", "phase") != "IdentityConflict" {
		t.Fatal("recreated workload adopted")
	}
}
func TestObserveAndLegacyOwnershipCannotStartWork(t *testing.T) {
	for _, mode := range []string{"Observe", "Execute"} {
		t.Run(mode, func(t *testing.T) {
			run, work := initialObjects(mode)
			objs := []client.Object{run, work}
			if mode == "Execute" {
				nr := named("NodeRequest", "ns", "legacy")
				nr.SetUID("legacy")
				nr.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"name": "a"}}
				objs = append(objs, nr)
			}
			c := frameworkClient(objs...)
			r := &RunReconciler{Client: c}
			for i := 0; i < 2; i++ {
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key("ns", "a")}); err != nil {
					t.Fatal(err)
				}
			}
			items, err := listItems(context.Background(), c, "policy.karmada.io/v1alpha1", "PropagationPolicy", "ns")
			if err != nil || len(items) != 0 {
				t.Fatal("unexpected placement write")
			}
		})
	}
}
func TestShortageTimerSurvivesReconcileWithoutPaidAllocation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	run, work := initialObjects("Observe")
	var s RunSpec
	_ = decodeSpec(run, &s)
	w := int64(1800)
	s.WaitSeconds = &w
	s.Cloud = CloudSpec{Allowed: true, Cluster: "aws1", TemplateRef: "reviewed", RiskProfileRef: "risk", MaxWorkers: 1, Deadline: stamp(now.Add(time.Hour))}
	run.Object["spec"] = encode(s)
	b := object("work.karmada.io/v1alpha2", "ResourceBinding")
	b.SetName("a")
	b.SetNamespace("ns")
	b.Object["spec"] = map[string]interface{}{"resource": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "a", "namespace": "ns", "uid": "source"}}
	b.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Scheduled", "status": "False", "message": "Insufficient nvidia.com/gpu"}}}
	c := frameworkClient(run, work, b)
	r := &RunReconciler{Client: c, Now: func() time.Time { return now }}
	req := ctrl.Request{NamespacedName: key("ns", "a")}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Get(ctx, req.NamespacedName, run)
	first := str(run, "status", "shortageSince")
	now = now.Add(21 * time.Minute)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, req.NamespacedName, run)
	if str(run, "status", "shortageSince") != first || !boolAt(run, "status", "activation", "prepareCloud") || str(run, "status", "phase") != "CloudActivationBlocked" {
		t.Fatalf("bad decision %v", run.Object["status"])
	}
}
func TestCheckpointPausesOwnedScheduleWhenTelemetryLost(t *testing.T) {
	run, _ := initialObjects("Execute")
	schedule := object("fluidcr.dcnlab.com/v1alpha1", "FluidCRMigration")
	schedule.SetName("a-periodic")
	schedule.SetNamespace("ns")
	setOwner(schedule, run)
	schedule.Object["spec"] = map[string]interface{}{"schedule": map[string]interface{}{"enabled": true, "intervalSeconds": int64(600)}}
	c := frameworkClient(run, schedule)
	r := &FrameworkCheckpointReconciler{Client: c}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key("ns", "a")}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), key("ns", "a-periodic"), schedule)
	if boolAt(schedule, "spec", "schedule", "enabled") {
		t.Fatal("schedule was not paused")
	}
	// Foreign schedules are not changed, even when the name matches.
	schedule.SetOwnerReferences([]metav1.OwnerReference{})
	_ = unstructured.SetNestedField(schedule.Object, true, "spec", "schedule", "enabled")
	_ = c.Update(context.Background(), schedule)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key("ns", "a")}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), key("ns", "a-periodic"), schedule)
	if !boolAt(schedule, "spec", "schedule", "enabled") {
		t.Fatal("foreign schedule changed")
	}
}

func TestOwnedObjectPreservesServerDefaultsWithoutRewrite(t *testing.T) {
	ctx := context.Background()
	run, _ := initialObjects("Execute")
	pp := object("policy.karmada.io/v1alpha1", "PropagationPolicy")
	pp.SetNamespace("ns")
	pp.SetName("placement")
	setOwner(pp, run)
	pp.Object["spec"] = map[string]interface{}{"placement": map[string]interface{}{"clusterAffinity": map[string]interface{}{"clusterNames": []interface{}{"onpre1"}}}, "activationPreference": "Immediate"}
	c := frameworkClient(run, pp)
	_ = c.Get(ctx, key("ns", "placement"), pp)
	version := pp.GetResourceVersion()
	desired := pp.DeepCopy()
	delete(desired.Object["spec"].(map[string]interface{}), "activationPreference")
	if err := ensureSpec(ctx, c, desired, run); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, key("ns", "placement"), pp)
	if pp.GetResourceVersion() != version || str(pp, "spec", "activationPreference") != "Immediate" {
		t.Fatal("server default removed or needless write performed")
	}
}
