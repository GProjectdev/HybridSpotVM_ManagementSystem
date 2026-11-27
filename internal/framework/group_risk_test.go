package framework

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGroupRiskInitialAndOperatingDecisions(t *testing.T) {
	run, s, cm, risk, now := cloudPolicyFixture()
	_, work := initialObjects("Execute")
	s.WorldSize = 4
	s.Cloud.MaxWorkers = 7
	s.ResourcePolicy = ResourcePolicySpec{Method: p.GroupRiskMethod, MinOnDemand: 0, GroupRisk: &p.GroupRiskPolicy{MinimumSurvivalProbability: .8, HazardBasis: p.IndependentPerInstanceHazard}}
	run.Object["spec"] = encode(s)
	cap := named("TrainingCapacity", "ns", initialCapacityName(run))
	cap.Object["status"] = map[string]interface{}{"templateJSON": cm.Data["nodeProvisionSpec.json"]}
	c := frameworkClient(run, work, cm, risk, cap)
	r := &RunReconciler{Client: c, Now: func() time.Time { return now }}
	ctx := context.Background()
	// Same calculation for bootstrap and operating updates, in both risk directions.
	for _, tc := range []struct {
		lambda float64
		spot   int64
	}{{0, 4}, {.05, 4}, {.06, 3}, {.08, 2}, {.15, 1}, {.25, 0}, {.08, 2}, {0, 4}} {
		_ = unstructured.SetNestedField(risk.Object, tc.lambda, "status", "lambdaPerHour")
		if err := c.Update(ctx, risk); err != nil {
			t.Fatal(err)
		}
		initial, _, err := initialCloudDecision(ctx, c, run, s, now)
		if err != nil {
			t.Fatal(err)
		}
		d, receipt, err := r.operatingCloudDecision(ctx, run, work, cap, s, WorkloadObservation{SelectedCluster: "aws1", Running: true, Runtime: p.RuntimeSnapshot{WorldSize: 4}})
		if err != nil || initial.SpotWorkers != tc.spot || d.SpotWorkers != tc.spot || d.OnDemandWorkers+d.SpotWorkers != 4 || d.GroupRisk == nil || d.CostEvaluated {
			t.Fatal(tc, initial, d, err)
		}
		decision := receipt["decision"].(map[string]interface{})
		if _, ok := decision["groupRisk"]; !ok {
			t.Fatal("group assumptions/candidates not recorded", receipt)
		}
		if receipt["riskSource"] == nil || receipt["riskUID"] != "risk-uid" {
			t.Fatal("risk provenance lost", receipt)
		}
	}
	// Fake-client checks must never launch provision/transition work by evaluating policy.
	for _, kind := range []string{"TrainingTransition", "NodeProvision"} {
		api := "training.dcnlab.com/v1alpha1"
		if kind == "NodeProvision" {
			api = "ml.dcn.ssu.ac.kr/v1alpha1"
		}
		items, err := listItems(ctx, c, api, kind, "ns")
		if err != nil || len(items) != 0 {
			t.Fatal(kind, items, err)
		}
	}
}

func TestGroupRiskChangeCreatesOnlyRequiredAdditiveReplacement(t *testing.T) {
	ctx := context.Background()
	run, s, cm, risk, now := cloudPolicyFixture()
	_, work := initialObjects("Execute")
	s.WorldSize = 4
	s.Cloud.MaxWorkers = 7
	s.ResourcePolicy = ResourcePolicySpec{Method: p.GroupRiskMethod, GroupRisk: &p.GroupRiskPolicy{MinimumSurvivalProbability: .8, HazardBasis: p.IndependentPerInstanceHazard}}
	run.Object["spec"] = encode(s)
	_ = unstructured.SetNestedField(work.Object, int64(4), "spec", "replicas")
	_ = unstructured.SetNestedField(risk.Object, .08, "status", "lambdaPerHour")
	cap := named("TrainingCapacity", "ns", initialCapacityName(run))
	cap.SetUID("capacity")
	cap.SetGeneration(1)
	setOwner(cap, run)
	cs := CapacitySpec{RunRef: ObjectRef{Name: run.GetName(), UID: string(run.GetUID())}, Cluster: s.Cloud.Cluster, TemplateRef: s.Cloud.TemplateRef, MaxWorkers: s.Cloud.MaxWorkers, Deadline: s.Cloud.Deadline}
	entries := map[string]interface{}{}
	pods := map[string]interface{}{}
	nodes := map[string]interface{}{}
	memberObjects := []client.Object{}
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("slot-%d", i)
		node := fmt.Sprintf("node-%d", i)
		pod := fmt.Sprintf("a-%d", i)
		cs.Allocations = append(cs.Allocations, Allocation{Name: name, Market: "spot", State: "Retain"})
		entries[name] = map[string]interface{}{"phase": "Ready", "nodeName": node, "nodeUID": node + "-uid", "memberUID": name + "-uid", "instanceID": fmt.Sprintf("i-%d", i)}
		pods[pod] = pod + "-uid"
		nodes[pod] = node
		memberObjects = append(memberObjects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, UID: types.UID(node + "-uid")}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}})
	}
	cap.Object["spec"] = encode(cs)
	cap.Object["status"] = map[string]interface{}{"templateJSON": cm.Data["nodeProvisionSpec.json"], "observedGeneration": int64(1), "observedAt": stamp(now), "allocations": entries}
	run.Object["status"] = map[string]interface{}{"workloadUID": string(work.GetUID()), "committedCluster": s.Cloud.Cluster, "executionPermit": map[string]interface{}{"workloadUID": string(work.GetUID()), "cluster": s.Cloud.Cluster, "memberWorkloadUID": "member-work", "pods": pods, "nodes": nodes, "issuedAt": stamp(now.Add(-time.Hour))}}
	c := frameworkClient(run, work, cap, cm, risk)
	m := frameworkClient(memberObjects...)
	r := &RunReconciler{Client: c, Now: func() time.Time { return now }, Members: func(string) (client.Reader, error) { return m, nil }}
	status, _, _ := unstructured.NestedMap(run.Object, "status")
	started, err := r.transitionTrigger(ctx, run, work, cap, s, WorkloadObservation{SelectedCluster: s.Cloud.Cluster, Running: true, Runtime: p.RuntimeSnapshot{WorldSize: 4}}, status)
	if err != nil || !started {
		t.Fatal(started, err)
	}
	tr, err := activeTransition(ctx, c, run)
	if err != nil || tr == nil {
		t.Fatal(tr, err)
	}
	var ts TransitionSpec
	if err := decodeSpec(tr, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Mode != "PlannedReplacement" || len(ts.AddedAllocations) != 2 || len(ts.RetireAllocations) != 2 || len(ts.TargetAllocations) != 4 {
		t.Fatal(ts)
	}
	for _, a := range ts.AddedAllocations {
		if a.Market != "on-demand" || a.State != "Prepare" {
			t.Fatal(a)
		}
	}
	before := cap.DeepCopy()
	if err := c.Get(ctx, client.ObjectKeyFromObject(cap), cap); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Object["spec"], cap.Object["spec"]) {
		t.Fatal("policy decision prematurely changed source capacity")
	}
	tr.SetUID("transition")
	if err := c.Update(ctx, tr); err != nil {
		t.Fatal(err)
	}
	phase, _, err := r.reconcileTransitionIntent(ctx, run, work, tr, s, status)
	if err != nil || phase != "PreparingTransitionCapacity" {
		t.Fatal(phase, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cap), cap); err != nil {
		t.Fatal(err)
	}
	if err := decodeSpec(cap, &cs); err != nil {
		t.Fatal(err)
	}
	if len(cs.Allocations) != 6 {
		t.Fatal(cs)
	}
	for _, a := range cs.Allocations {
		if a.State == "Retire" || a.State == "Fence" {
			t.Fatal("old workers removed before restore", a)
		}
	}
}

func TestGroupRiskFrameworkRejectsInvalidMethodAndKeepsDefault(t *testing.T) {
	run, s, cm, risk, now := cloudPolicyFixture()
	c := frameworkClient(run, cm, risk)
	if d, _, err := initialCloudDecision(context.Background(), c, run, s, now); err != nil || d.VMPolicySource != "desai-section-4.3-equations-6-7-fixed-world" {
		t.Fatal(d, err)
	}
	for _, policy := range []ResourcePolicySpec{
		{Method: "typo"},
		{Method: p.GroupRiskMethod},
		{Method: p.GroupRiskMethod, GroupRisk: &p.GroupRiskPolicy{MinimumSurvivalProbability: .8, HazardBasis: "aggregate"}},
		{Method: "paper-eq6-7", GroupRisk: &p.GroupRiskPolicy{MinimumSurvivalProbability: .8, HazardBasis: p.IndependentPerInstanceHazard}},
	} {
		s.ResourcePolicy = policy
		if s.Validate() == nil {
			t.Fatal("invalid Run accepted", policy)
		}
		if _, _, err := initialCloudDecision(context.Background(), c, run, s, now); err == nil {
			t.Fatal("invalid contract allocated", policy)
		}
	}
}

// Simulate an informer observation changing between bootstrap validation and
// the operating-policy read. Decisions must not carry another snapshot's receipt.
type changingRiskClient struct {
	client.Client
	riskReads int
}

func (c *changingRiskClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "SpotRiskProfile" {
		c.riskReads++
		if c.riskReads == 2 {
			u.SetResourceVersion("changed")
			_ = unstructured.SetNestedField(u.Object, .3, "status", "lambdaPerHour")
		}
	}
	return nil
}

func TestOperatingPolicyRejectsMixedRiskSnapshots(t *testing.T) {
	for _, method := range []string{"paper-eq6-7", p.GroupRiskMethod} {
		run, s, cm, risk, now := cloudPolicyFixture()
		_, work := initialObjects("Execute")
		s.ResourcePolicy.Method = method
		if method == p.GroupRiskMethod {
			s.ResourcePolicy.GroupRisk = &p.GroupRiskPolicy{MinimumSurvivalProbability: .8, HazardBasis: p.IndependentPerInstanceHazard}
		}
		cap := named("TrainingCapacity", "ns", initialCapacityName(run))
		cap.Object["status"] = map[string]interface{}{"templateJSON": cm.Data["nodeProvisionSpec.json"]}
		c := &changingRiskClient{Client: frameworkClient(run, work, cm, risk, cap)}
		r := &RunReconciler{Client: c, Now: func() time.Time { return now }}
		_, receipt, err := r.operatingCloudDecision(context.Background(), run, work, cap, s, WorkloadObservation{SelectedCluster: "aws1", Running: true})
		if err == nil || receipt != nil {
			t.Fatal("mixed snapshot decision accepted", method, receipt, err)
		}
	}
}
