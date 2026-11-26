package framework

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	quantity "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func capacityFixture(t *testing.T) (*CapacityReconciler, client.Client, client.Client, *unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	run, _ := initialObjects("Execute")
	var rs RunSpec
	_ = decodeSpec(run, &rs)
	rs.ExecutionGate = true
	rs.Cloud = CloudSpec{Allowed: true, Cluster: "aws1", TemplateRef: "gpu", RiskProfileRef: "risk", MaxWorkers: 1, Deadline: "2026-11-26T10:00:00Z"}
	run.Object["spec"] = encode(rs)
	cap := named("TrainingCapacity", "ns", "cap")
	cap.SetUID("capacity")
	cap.SetGeneration(1)
	setOwner(cap, run)
	cap.Object["spec"] = encode(CapacitySpec{RunRef: ObjectRef{Name: "a", UID: "run"}, Cluster: "aws1", TemplateRef: "gpu", MaxWorkers: 1, Deadline: rs.Cloud.Deadline, Allocations: []Allocation{{Name: "worker", Market: "spot", State: "Prepare"}}})
	immutable := true
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: "ns", UID: "template"}, Immutable: &immutable, Data: map[string]string{"nodeProvisionSpec.json": `{"provider":"AWS","networkMode":"VPC","hardwareType":"gpu","instanceType":"g5.2xlarge","region":"ap-northeast-2"}`}}
	c := frameworkClient(run, cap, cm)
	m := frameworkClient()
	r := &CapacityReconciler{Client: c, Members: func(cluster string) (client.Reader, error) {
		if cluster != "aws1" {
			t.Fatal(cluster)
		}
		return m, nil
	}, Now: func() time.Time { return time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC) }}
	return r, c, m, cap, run
}
func reconcileCapacity(t *testing.T, r *CapacityReconciler, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key("ns", "cap")}); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCapacityPinsIdentityBeforeProvisioningAndPreservesMarket(t *testing.T) {
	r, c, _, cap, _ := capacityFixture(t)
	reconcileCapacity(t, r, 3)
	np := p.NewObject("NodeProvision")
	if err := c.Get(context.Background(), key("ns", "worker"), np); err != nil {
		t.Fatal(err)
	}
	if str(np, "spec", "marketType") != "Spot" || !owned(np, cap) {
		t.Fatal("wrong market or owner")
	}
	policies, _ := listItems(context.Background(), c, "policy.karmada.io/v1alpha1", "PropagationPolicy", "ns")
	if len(policies) != 0 {
		t.Fatal("dispatch preceded durable receipt")
	}
	// Kubernetes assigns this UID. The fake client needs an explicit server identity.
	np.SetUID("origin")
	if err := c.Update(context.Background(), np); err != nil {
		t.Fatal(err)
	}
	// Drop fake's empty receipt as if recovering from a create response loss.
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	unstructured.RemoveNestedField(cap.Object, "status", "allocations", "worker", "originUID")
	_ = c.Status().Update(context.Background(), cap)
	reconcileCapacity(t, r, 3)
	policies, _ = listItems(context.Background(), c, "policy.karmada.io/v1alpha1", "PropagationPolicy", "ns")
	if len(policies) != 1 {
		t.Fatalf("expected one placement: %d", len(policies))
	}
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "allocations", "worker", "originUID") != "origin" || str(cap, "status", "templateSHA256") == "" {
		t.Fatal("identity not pinned")
	}
}
func TestCapacityRejectsUnauthorizedOwnerAndLimitBeforeCreation(t *testing.T) {
	for _, kind := range []string{"owner", "limit", "suspend"} {
		t.Run(kind, func(t *testing.T) {
			r, c, _, cap, run := capacityFixture(t)
			if kind == "owner" {
				_ = unstructured.SetNestedField(cap.Object, "foreign", "spec", "runRef", "uid")
			}
			if kind == "limit" {
				_ = unstructured.SetNestedSlice(cap.Object, []interface{}{map[string]interface{}{"name": "one", "market": "spot", "state": "Prepare"}, map[string]interface{}{"name": "two", "market": "spot", "state": "Retain"}}, "spec", "allocations")
			}
			if kind == "suspend" {
				_ = unstructured.SetNestedField(run.Object, true, "spec", "suspend")
				_ = c.Update(context.Background(), run)
			}
			_ = c.Update(context.Background(), cap)
			reconcileCapacity(t, r, 5)
			nodes, _ := listItems(context.Background(), c, "ml.dcn.ssu.ac.kr/v1alpha1", "NodeProvision", "ns")
			if len(nodes) != 0 {
				t.Fatal("unauthorized allocation created")
			}
		})
	}
}
func TestExpiredUnstartedCapacityDoesNotLaunchToCleanUp(t *testing.T) {
	r, c, _, cap, _ := capacityFixture(t)
	r.Now = func() time.Time { return time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC) }
	reconcileCapacity(t, r, 5)
	nodes, _ := listItems(context.Background(), c, "ml.dcn.ssu.ac.kr/v1alpha1", "NodeProvision", "ns")
	if len(nodes) != 0 {
		t.Fatal("expired allocation created")
	}
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "phase") != "Retired" {
		t.Fatal(cap.Object["status"])
	}
}
func TestCapacityNeverAdoptsForeignProvision(t *testing.T) {
	r, c, _, cap, _ := capacityFixture(t)
	np := p.NewObject("NodeProvision")
	np.SetName("worker")
	np.SetNamespace("ns")
	np.SetUID("foreign")
	np.Object["spec"] = map[string]interface{}{"marketType": "OnDemand"}
	_ = c.Create(context.Background(), np)
	reconcileCapacity(t, r, 4)
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "phase") != "OwnershipConflict" {
		t.Fatal(cap.Object["status"])
	}
	_ = c.Get(context.Background(), key("ns", "worker"), np)
	if str(np, "spec", "marketType") != "OnDemand" || len(np.GetOwnerReferences()) != 0 {
		t.Fatal("foreign resource changed")
	}
}

func readyCapacityFixture(t *testing.T) (*CapacityReconciler, client.Client, client.Client, *unstructured.Unstructured, *unstructured.Unstructured) {
	r, c, m, cap, _ := capacityFixture(t)
	reconcileCapacity(t, r, 2)
	np := p.NewObject("NodeProvision")
	np.SetName("worker")
	np.SetNamespace("ns")
	np.SetUID("origin")
	np.SetGeneration(1)
	setOwner(np, cap)
	np.SetLabels(map[string]string{RunUIDLabel: "run", CapacityUIDLabel: "capacity", originProvisionUID: "origin"})
	np.Object["spec"] = map[string]interface{}{"provider": "AWS", "networkMode": "VPC", "marketType": "Spot"}
	_ = c.Create(context.Background(), np)
	member := np.DeepCopy()
	member.SetUID("member")
	member.SetResourceVersion("")
	member.SetOwnerReferences(nil)
	member.Object["status"] = map[string]interface{}{"phase": "Ready", "instanceId": "i-owned", "nodeName": "gpu"}
	_ = m.Create(context.Background(), member)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu", UID: "node", Labels: map[string]string{"ml.dcn.ssu.ac.kr/node-provision-uid": "member"}}, Spec: corev1.NodeSpec{ProviderID: "aws:///i-owned"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, Allocatable: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): quantity.MustParse("1")}}}
	_ = m.Create(context.Background(), node)
	reconcileCapacity(t, r, 4)
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "phase") != "Ready" {
		t.Fatal(cap.Object["status"])
	}
	return r, c, m, cap, member
}
func TestRetirementNeedsCurrentProviderReceiptAndMemberDisappearance(t *testing.T) {
	r, c, m, cap, member := readyCapacityFixture(t)
	_ = unstructured.SetNestedSlice(cap.Object, []interface{}{map[string]interface{}{"name": "worker", "market": "spot", "state": "Retire"}}, "spec", "allocations")
	_ = c.Update(context.Background(), cap)
	reconcileCapacity(t, r, 1)
	origin := p.NewObject("NodeProvision")
	_ = c.Get(context.Background(), key("ns", "worker"), origin)
	if str(origin, "spec", "fence", "instanceID") != "i-owned" || str(origin, "spec", "fence", "operationUID") != "capacity" {
		t.Fatal("missing bound provider fence")
	}
	member.SetGeneration(2)
	_ = unstructured.SetNestedMap(member.Object, map[string]interface{}{"phase": "Fenced", "operationUID": "capacity", "instanceID": "i-owned", "observedGeneration": int64(1), "observedAt": "2026-11-26T04:00:00Z"}, "status", "fence")
	_ = m.Update(context.Background(), member)
	reconcileCapacity(t, r, 2)
	if err := c.Get(context.Background(), key("ns", "worker"), origin); err != nil {
		t.Fatal("stale receipt permitted deletion", err)
	}
	_ = unstructured.SetNestedField(member.Object, int64(2), "status", "fence", "observedGeneration")
	_ = m.Update(context.Background(), member)
	reconcileCapacity(t, r, 1)
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "allocations", "worker", "fencedAt") == "" {
		t.Fatal("receipt not preserved")
	}
	// A lost template must not block cleanup of already-owned allocations.
	template := &corev1.ConfigMap{}
	_ = c.Get(context.Background(), key("ns", "gpu"), template)
	_ = c.Delete(context.Background(), template)
	reconcileCapacity(t, r, 2)
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "phase") == "Retired" {
		t.Fatal("member still exists but cleanup marked complete")
	}
	_ = m.Delete(context.Background(), member)
	reconcileCapacity(t, r, 1)
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "phase") != "Retired" {
		t.Fatal(cap.Object["status"])
	}
}
func TestCapacityRejectsRecreatedMemberAndWrongNodeOwner(t *testing.T) {
	for _, kind := range []string{"member", "node"} {
		t.Run(kind, func(t *testing.T) {
			r, c, m, cap, member := readyCapacityFixture(t)
			if kind == "member" {
				_ = m.Delete(context.Background(), member)
				member.SetResourceVersion("")
				member.SetUID("replacement")
				_ = m.Create(context.Background(), member)
			} else {
				node := &corev1.Node{}
				_ = m.Get(context.Background(), client.ObjectKey{Name: "gpu"}, node)
				node.Labels["ml.dcn.ssu.ac.kr/node-provision-uid"] = "foreign"
				_ = m.Update(context.Background(), node)
			}
			reconcileCapacity(t, r, 1)
			_ = c.Get(context.Background(), key("ns", "cap"), cap)
			if str(cap, "status", "phase") != "IdentityConflict" {
				t.Fatal(cap.Object["status"])
			}
		})
	}
}

func TestDeletedRunRequestsCleanupInsteadOfNewCapacity(t *testing.T) {
	r, c, _, cap, _ := readyCapacityFixture(t)
	run := named("TrainingRun", "ns", "a")
	_ = c.Get(context.Background(), key("ns", "a"), run)
	_ = c.Delete(context.Background(), run)
	reconcileCapacity(t, r, 1)
	origin := p.NewObject("NodeProvision")
	_ = c.Get(context.Background(), key("ns", "worker"), origin)
	if str(origin, "spec", "fence", "instanceID") != "i-owned" {
		t.Fatal("orphaned approved allocation did not request cleanup")
	}
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "phase") != "Retiring" {
		t.Fatal(cap.Object["status"])
	}
}

func TestChangedRunCannotBlockAuthorizedRetirement(t *testing.T) {
	for _, reason := range []string{"deadline", "explicit-retire", "recreated-owner"} {
		t.Run(reason, func(t *testing.T) {
			r, c, _, cap, _ := readyCapacityFixture(t)
			run := named("TrainingRun", "ns", "a")
			_ = c.Get(context.Background(), key("ns", "a"), run)
			if reason == "recreated-owner" {
				_ = c.Delete(context.Background(), run)
				run.SetUID("different-owner")
				run.SetResourceVersion("")
				_ = c.Create(context.Background(), run)
			} else {
				_ = unstructured.SetNestedField(run.Object, false, "spec", "cloud", "allowed")
				_ = c.Update(context.Background(), run)
				if reason == "deadline" {
					r.Now = func() time.Time { return time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC) }
				} else {
					allocations, _, _ := unstructured.NestedSlice(cap.Object, "spec", "allocations")
					allocations[0].(map[string]interface{})["state"] = "Retire"
					_ = unstructured.SetNestedSlice(cap.Object, allocations, "spec", "allocations")
					_ = c.Update(context.Background(), cap)
				}
			}
			reconcileCapacity(t, r, 1)
			origin := p.NewObject("NodeProvision")
			_ = c.Get(context.Background(), key("ns", "worker"), origin)
			if str(origin, "spec", "fence", "instanceID") != "i-owned" {
				t.Fatal("authorized cleanup blocked by parent changes")
			}
			_ = c.Get(context.Background(), key("ns", "cap"), cap)
			if str(cap, "status", "phase") != "Retiring" {
				t.Fatal(cap.Object["status"])
			}
		})
	}
}

func TestChangedRunDoesNotAuthorizeNewAllocationFromSnapshot(t *testing.T) {
	r, c, _, cap, run := capacityFixture(t)
	reconcileCapacity(t, r, 2) // approval and template pinned; no VM request yet
	_ = unstructured.SetNestedField(run.Object, false, "spec", "cloud", "allowed")
	_ = c.Update(context.Background(), run)
	reconcileCapacity(t, r, 2)
	nodes, _ := listItems(context.Background(), c, "ml.dcn.ssu.ac.kr/v1alpha1", "NodeProvision", "ns")
	if len(nodes) != 0 {
		t.Fatal("old approval authorized a fresh allocation after revocation")
	}
	_ = c.Get(context.Background(), key("ns", "cap"), cap)
	if str(cap, "status", "phase") != "OwnerConflict" {
		t.Fatal(cap.Object["status"])
	}
}
