package framework

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cloudPolicyFixture() (*unstructured.Unstructured, RunSpec, *corev1.ConfigMap, *unstructured.Unstructured, time.Time) {
	run, _ := initialObjects("Execute")
	var s RunSpec
	_ = decodeSpec(run, &s)
	now := time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC)
	s.WorldSize = 2
	s.ExecutionGate = true
	s.ResourcePolicy.MinOnDemand = 1
	s.Cloud = CloudSpec{Allowed: true, Cluster: "aws1", TemplateRef: "template", RiskProfileRef: "risk", MaxWorkers: 3, Deadline: stamp(now.Add(time.Hour))}
	run.Object["spec"] = encode(s)
	immutable := true
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "ns", UID: "template-uid"}, Immutable: &immutable, Data: map[string]string{"nodeProvisionSpec.json": `{"provider":"AWS","networkMode":"VPC","hardwareType":"gpu","region":"ap-northeast-2","instanceType":"g5.2xlarge","awsConfig":{"availabilityZone":"ap-northeast-2c"}}`}}
	risk := p.NewObject("SpotRiskProfile")
	risk.SetName("risk")
	risk.SetNamespace("ns")
	risk.SetUID("risk-uid")
	risk.SetGeneration(2)
	risk.Object["spec"] = map[string]interface{}{"provider": "AWS", "region": "ap-northeast-2", "availabilityZone": "ap-northeast-2c", "instanceType": "g5.2xlarge"}
	risk.Object["status"] = map[string]interface{}{"ready": true, "observedGeneration": int64(2), "observedAt": stamp(now), "validUntil": stamp(now.Add(time.Hour)), "source": map[string]interface{}{"type": "unit-test-controlled-input", "feed": map[string]interface{}{"experimental": true}}, "lambdaPerHour": 0.001, "prices": map[string]interface{}{"spotPricePerHour": 0.6, "onDemandPricePerHour": 1.5}}
	return run, s, cm, risk, now
}

func TestInitialCloudPolicyReusesPaperAndRejectsInvalidEvidence(t *testing.T) {
	for _, kind := range []string{"valid", "high-risk", "wrong-market", "stale", "future", "no-price", "new-generation", "invalid-alpha"} {
		t.Run(kind, func(t *testing.T) {
			run, s, cm, risk, now := cloudPolicyFixture()
			switch kind {
			case "high-risk":
				_ = unstructured.SetNestedField(risk.Object, 1.0, "status", "lambdaPerHour")
			case "wrong-market":
				_ = unstructured.SetNestedField(risk.Object, "ap-northeast-2a", "spec", "availabilityZone")
			case "stale":
				_ = unstructured.SetNestedField(risk.Object, stamp(now.Add(-time.Second)), "status", "validUntil")
			case "future":
				_ = unstructured.SetNestedField(risk.Object, stamp(now.Add(time.Minute)), "status", "observedAt")
			case "no-price":
				unstructured.RemoveNestedField(risk.Object, "status", "prices")
			case "new-generation":
				risk.SetGeneration(3)
			case "invalid-alpha":
				a := 1.1
				s.ResourcePolicy.Alpha = &a
			}
			c := frameworkClient(run, cm, risk)
			d, receipt, err := initialCloudDecision(context.Background(), c, run, s, now)
			if kind != "valid" && kind != "high-risk" {
				if err == nil {
					t.Fatal("untrusted input authorized allocation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if receipt["riskSource"].(map[string]interface{})["type"] != "unit-test-controlled-input" {
				t.Fatal("risk provenance lost", receipt)
			}
			if receipt["riskUID"] != "risk-uid" || d.VMPolicySource != "desai-section-4.3-equations-6-7-fixed-world" {
				t.Fatal(receipt, d)
			}
			if kind == "valid" && (d.SpotWorkers != 1 || d.OnDemandWorkers != 1) {
				t.Fatal(d)
			}
			if kind == "high-risk" && (d.SpotWorkers != 0 || d.OnDemandWorkers != 2) {
				t.Fatal(d)
			}
		})
	}
}

func TestInitialCapacityPinsCompositionAndCancellationDoesNotRecreate(t *testing.T) {
	run, s, cm, risk, now := cloudPolicyFixture()
	c := frameworkClient(run, cm, risk)
	r := &RunReconciler{Client: c, Now: func() time.Time { return now }}
	status := map[string]interface{}{}
	cap, err := r.initialCapacity(context.Background(), run, s, status, false)
	if err != nil || cap != nil {
		t.Fatal("read-only observation created capacity", err)
	}
	cap, err = r.initialCapacity(context.Background(), run, s, status, true)
	if err != nil {
		t.Fatal(err)
	}
	cap.SetUID("capacity-uid")
	_ = c.Update(context.Background(), cap)
	cap, err = r.initialCapacity(context.Background(), run, s, status, false)
	if err != nil {
		t.Fatal(err)
	}
	var cs CapacitySpec
	_ = decodeSpec(cap, &cs)
	if len(cs.Allocations) != 2 || cs.Allocations[0].Market != "on-demand" || cs.Allocations[1].Market != "spot" {
		t.Fatal(cs)
	}
	// A changed forecast cannot rewrite an in-flight allocation in place.
	_ = unstructured.SetNestedField(risk.Object, 1.0, "status", "lambdaPerHour")
	_ = c.Update(context.Background(), risk)
	cap, err = r.initialCapacity(context.Background(), run, s, status, true)
	if err != nil {
		t.Fatal(err)
	}
	var after CapacitySpec
	_ = decodeSpec(cap, &after)
	if after.Allocations[1].Market != "spot" {
		t.Fatal("policy update overwrote active allocation")
	}
	if err = retireUnusedCapacity(context.Background(), c, cap); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cap), cap)
	_ = decodeSpec(cap, &after)
	for _, a := range after.Allocations {
		if a.State != "Retire" {
			t.Fatal("unused capacity retained")
		}
	}
	_ = c.Delete(context.Background(), cap)
	if _, err = r.initialCapacity(context.Background(), run, s, status, true); err == nil {
		t.Fatal("deleted capacity recreated")
	}
}

func TestCloudPlacementPreservesUserAffinityAndRestrictsOwnedNodes(t *testing.T) {
	run, work := initialObjects("Execute")
	s := RunSpec{WorldSize: 1, Cloud: CloudSpec{Cluster: "aws1"}}
	_ = unstructured.SetNestedMap(work.Object, map[string]interface{}{"nodeAffinity": map[string]interface{}{"requiredDuringSchedulingIgnoredDuringExecution": map[string]interface{}{"nodeSelectorTerms": []interface{}{map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{"key": "example.com/gpu", "operator": "In", "values": []interface{}{"a10"}}}}}}}}, "spec", "template", "spec", "affinity")
	cap := named("TrainingCapacity", "ns", "cap")
	cap.Object["spec"] = encode(CapacitySpec{Allocations: []Allocation{{Name: "worker", Market: "spot", State: "Prepare"}}})
	cap.Object["status"] = map[string]interface{}{"allocations": map[string]interface{}{"worker": map[string]interface{}{"memberUID": "member-uid"}}}
	c := frameworkClient(run, work, cap)
	if err := ensureCloudNodePlacement(context.Background(), c, run, work, cap, s); err != nil {
		t.Fatal(err)
	}
	o := object("policy.karmada.io/v1alpha1", "OverridePolicy")
	_ = c.Get(context.Background(), key("ns", "a-cloud-nodes"), o)
	rules, _, _ := unstructured.NestedSlice(o.Object, "spec", "overrideRules")
	patches, _, _ := unstructured.NestedSlice(rules[0].(map[string]interface{}), "overriders", "plaintext")
	value := patches[0].(map[string]interface{})["value"].(map[string]interface{})
	terms, _, _ := unstructured.NestedSlice(value, "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	expr, _, _ := unstructured.NestedSlice(terms[0].(map[string]interface{}), "matchExpressions")
	if len(expr) != 2 || expr[0].(map[string]interface{})["key"] != "example.com/gpu" || expr[1].(map[string]interface{})["key"] != "ml.dcn.ssu.ac.kr/node-provision-uid" {
		t.Fatal(expr)
	}
}

func TestRunPrewarmsOnlyAfterLeadTimeAndCancelsOnPremCommit(t *testing.T) {
	ctx := context.Background()
	run, s, cm, risk, now := cloudPolicyFixture()
	wait := int64(1800)
	s.WaitSeconds = &wait
	run.Object["spec"] = encode(s)
	_, work := initialObjects("Execute")
	_ = unstructured.SetNestedField(work.Object, int64(2), "spec", "replicas")
	// Existing valid gate avoids test-only injection details, but every reconcile
	// still checks its template and credential ownership.
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gate-source", Namespace: "ns"}, Data: map[string][]byte{"kubeconfig": []byte("test-credential")}}
	c := frameworkClient(run, work, cm, risk, secret)
	r := &RunReconciler{Client: c, Now: func() time.Time { return now }, GateImage: "example/gate@sha256:unit-test", GateCredentialSecret: "gate-source"}
	req := ctrl.Request{NamespacedName: key("ns", "a")}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	binding := object("work.karmada.io/v1alpha2", "ResourceBinding")
	binding.SetName("work")
	binding.SetNamespace("ns")
	binding.Object["spec"] = map[string]interface{}{"resource": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "namespace": "ns", "name": "a", "uid": "source"}}
	binding.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Scheduled", "status": "False", "message": "Insufficient nvidia.com/gpu"}}}
	_ = c.Create(ctx, binding)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	now = now.Add(19 * time.Minute)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	caps, _ := listItems(ctx, c, "training.dcnlab.com/v1alpha1", "TrainingCapacity", "ns")
	if len(caps) != 0 {
		t.Fatal("prepared before W-lead")
	}
	now = now.Add(time.Minute)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	caps, _ = listItems(ctx, c, "training.dcnlab.com/v1alpha1", "TrainingCapacity", "ns")
	if len(caps) != 1 {
		_ = c.Get(ctx, req.NamespacedName, run)
		t.Fatal("capacity not created at W-lead", run.Object["status"])
	}
	_ = c.Get(ctx, req.NamespacedName, run)
	_ = unstructured.SetNestedField(run.Object, "onpre1", "status", "committedCluster")
	_ = c.Status().Update(ctx, run)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	cap := named("TrainingCapacity", "ns", caps[0].GetName())
	_ = c.Get(ctx, client.ObjectKeyFromObject(cap), cap)
	var cs CapacitySpec
	_ = decodeSpec(cap, &cs)
	for _, a := range cs.Allocations {
		if a.State != "Retire" {
			t.Fatal("OnPrem commit did not cancel unused Cloud")
		}
	}
}

func TestInitialWorkloadUsesResourceAwareSingleClusterScheduling(t *testing.T) {
	run, work := initialObjects("Execute")
	c := frameworkClient(run, work)
	if err := ensureWorkloadPlacement(context.Background(), c, run, work, []string{"onpre1", "aws1"}, true); err != nil {
		t.Fatal(err)
	}
	pp := object("policy.karmada.io/v1alpha1", "PropagationPolicy")
	if err := c.Get(context.Background(), key("ns", "a-training-placement"), pp); err != nil {
		t.Fatal(err)
	}
	if str(pp, "spec", "placement", "replicaScheduling", "replicaSchedulingType") != "Divided" || str(pp, "spec", "placement", "replicaScheduling", "replicaDivisionPreference") != "Aggregated" {
		t.Fatal(pp.Object)
	}
	groups, _, _ := unstructured.NestedSlice(pp.Object, "spec", "placement", "spreadConstraints")
	if groups[0].(map[string]interface{})["maxGroups"] != int64(1) {
		t.Fatal("world may split", groups)
	}
	if err := ensureWorkloadPlacement(context.Background(), c, run, work, []string{"aws1"}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key("ns", "a-training-placement"), pp); err != nil {
		t.Fatal(err)
	}
	if str(pp, "spec", "placement", "replicaScheduling", "replicaSchedulingType") != "Divided" {
		t.Fatal("commit unexpectedly removed scheduler contract")
	}
}
