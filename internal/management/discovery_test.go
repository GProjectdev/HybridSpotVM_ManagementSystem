package management

import (
	"context"
	"fmt"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func discoveryDefaultsYAML(extra string) string {
	return `riskProfileRef:
  name: train-risk
policy:
  alpha: 0.8
  minOnDemand: 1
  forecastHorizonSeconds: 3600
capacity:
  aws:
    karmadaCluster: aws
    region: test
    instanceType: test
    ami: ami-test
    vpcId: vpc-test
    subnetId: subnet-test
    credentialsRef:
      name: creds
` + extra
}

func discoveryFixture(t *testing.T, cluster string) (*DiscoveryReconciler, *unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	scheme := testScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sts := workloadFixture("workload-uid")
	sts.Object["spec"] = map[string]interface{}{"replicas": int64(2), "template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "trainer", "image": "test"}}}}}
	rb := bindingObject()
	rb.SetName("trainer-statefulset")
	rb.SetNamespace("default")
	rb.SetUID("rb-uid")
	rb.Object["spec"] = map[string]interface{}{"resource": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "namespace": "default", "uid": "workload-uid"}, "clusters": []interface{}{map[string]interface{}{"name": cluster, "replicas": int64(2)}}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: defaultsName, Namespace: "hybridspot-system"}, Data: map[string]string{"spec.yaml": discoveryDefaultsYAML("")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(p.NewObject("TrainingPolicy")).WithObjects(sts, rb, cm).Build()
	return &DiscoveryReconciler{Client: c, Reader: c}, sts, rb
}

func setDiscoveryDefaults(t *testing.T, r *DiscoveryReconciler, specYAML string) {
	t.Helper()
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: "hybridspot-system", Name: defaultsName}, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["spec.yaml"] = specYAML
	if err := r.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
}

func discover(t *testing.T, r *DiscoveryReconciler, sts *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	obj := p.NewObject("TrainingPolicy")
	if err := r.Get(ctx, types.NamespacedName{Namespace: sts.GetNamespace(), Name: autoName(sts.GetName())}, obj); err != nil {
		t.Fatal(err)
	}
	obj.SetUID("auto-policy-uid")
	obj.SetGeneration(1)
	if err := r.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestAutomaticPolicyReplacementDefaults(t *testing.T) {
	t.Run("omitted stays disabled", func(t *testing.T) {
		r, sts, _ := discoveryFixture(t, "aws")
		obj := discover(t, r, sts)
		if _, ok, _ := unstructured.NestedMap(obj.Object, "spec", "replacement"); ok {
			t.Fatal("omitted replacement defaults created an opt-in block")
		}
		if boolField(obj.Object, "spec", "replacement", "enabled") {
			t.Fatal("omitted replacement defaults enabled replacement")
		}
	})

	t.Run("explicit opt-in preserved", func(t *testing.T) {
		r, sts, _ := discoveryFixture(t, "aws")
		setDiscoveryDefaults(t, r, discoveryDefaultsYAML("replacement:\n  enabled: true\n"))
		obj := discover(t, r, sts)
		if !boolField(obj.Object, "spec", "replacement", "enabled") {
			t.Fatal("explicit replacement default was not preserved")
		}
	})
}

func TestAutomaticPolicyCreatesOneSpotOneOnDemand(t *testing.T) {
	r, sts, _ := discoveryFixture(t, "aws")
	obj := discover(t, r, sts)
	ctx := context.Background()
	if p.ReadPolicySpec(obj).MinOnDemand != 1 {
		t.Fatal("integer defaults lost")
	}
	now := time.Now().UTC()
	risk := riskFixture(now)
	_ = unstructured.SetNestedField(risk.Object, float64(.1), "status", "lambdaPerHour")
	if err := r.Create(ctx, risk); err != nil {
		t.Fatal(err)
	}
	pr := &PolicyReconciler{Client: r.Client, APIReader: r.Reader, Clock: func() time.Time { return now }}
	for i := 0; i < 2; i++ {
		if _, err := pr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}); err != nil {
			t.Fatal(err)
		}
	}
	list := p.NewList("NodeProvision")
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, np := range list.Items {
		counts[stringField(np.Object, "spec", "marketType")]++
	}
	if counts["Spot"] != 1 || counts["OnDemand"] != 1 || len(list.Items) != 2 {
		t.Fatalf("unexpected capacity: %v", counts)
	}
	requests := mapPolicies(r.Client, "risk")(ctx, risk)
	if len(requests) != 1 || requests[0].Name != obj.GetName() {
		t.Fatalf("risk watch mapping: %v", requests)
	}
}
func TestAutomaticNonAWSAndMigrationFreeze(t *testing.T) {
	r, sts, rb := discoveryFixture(t, "onpre1")
	ctx := context.Background()
	obj := discover(t, r, sts)
	pr := &PolicyReconciler{Client: r.Client, APIReader: r.Reader}
	if _, err := pr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}); err != nil {
		t.Fatal(err)
	}
	list := p.NewList("NodeProvision")
	if err := r.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatal("non-AWS placement allocated AWS VMs")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rb), rb); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(rb.Object, []interface{}{map[string]interface{}{"name": "aws", "replicas": int64(2)}}, "spec", "clusters")
	if err := r.Update(ctx, rb); err != nil {
		t.Fatal(err)
	}
	if _, err := automaticPlacement(ctx, r.Reader, obj); err == nil {
		t.Fatal("unsuspended transition accepted")
	}
	// Discovery must retain source even when the new target runtime is absent.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	if stringField(obj.Object, "spec", "sourceCluster") != "onpre1" {
		t.Fatal("source overwritten by target intent")
	}
	if stringField(obj.Object, "status", "discovery", "phase") != "UnsafePlacementChange" {
		t.Fatal("unsafe transition not reported")
	}
	_ = unstructured.SetNestedField(rb.Object, true, "spec", "suspension", "dispatching")
	if err := r.Update(ctx, rb); err != nil {
		t.Fatal(err)
	}
	if target, err := automaticPlacement(ctx, r.Reader, obj); err != nil || target != "aws" {
		t.Fatalf("suspended capacity preparation rejected: %s %v", target, err)
	}
}
func TestBindingRejectsStaleSplitPartialAndScale(t *testing.T) {
	for _, mode := range []string{"stale", "split", "partial", "zero"} {
		t.Run(mode, func(t *testing.T) {
			r, sts, rb := discoveryFixture(t, "aws")
			ctx := context.Background()
			switch mode {
			case "stale":
				_ = unstructured.SetNestedField(rb.Object, "old-uid", "spec", "resource", "uid")
			case "split":
				_ = unstructured.SetNestedSlice(rb.Object, []interface{}{map[string]interface{}{"name": "aws"}, map[string]interface{}{"name": "onpre1"}}, "spec", "clusters")
			case "partial":
				_ = unstructured.SetNestedSlice(rb.Object, []interface{}{map[string]interface{}{"name": "aws", "replicas": int64(1)}}, "spec", "clusters")
			case "zero":
				_ = unstructured.SetNestedField(sts.Object, int64(0), "spec", "replicas")
			}
			if err := r.Update(ctx, rb); err != nil {
				t.Fatal(err)
			}
			if _, _, err := selectedBinding(ctx, r.Reader, sts); err == nil {
				t.Fatal("unsafe binding accepted")
			}
		})
	}
}
func TestVerifiedTransitionRequiresCurrentRestoreAndLiveRuntime(t *testing.T) {
	r, sts, _ := discoveryFixture(t, "onpre1")
	ctx := context.Background()
	policy := discover(t, r, sts)
	if err := r.ensureRuntime(ctx, policy, sts, "aws"); err != nil {
		t.Fatal(err)
	}
	rt := p.NewObject("TrainingRuntime")
	if err := r.Get(ctx, types.NamespacedName{Namespace: "default", Name: runtimeName(policy.GetName(), "aws")}, rt); err != nil {
		t.Fatal(err)
	}
	rt.SetUID("target-runtime-uid")
	rt.SetGeneration(1)
	now := time.Now().UTC()
	pods := []interface{}{}
	for i := int64(0); i < 2; i++ {
		pods = append(pods, map[string]interface{}{"name": fmt.Sprintf("trainer-%d", i), "uid": fmt.Sprintf("pod-%d", i), "rank": i, "checkpointID": "ckpt-1", "observedAt": now.Format(time.RFC3339)})
	}
	rt.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws", "status": map[string]interface{}{"phase": "Running", "workloadUID": "workload-uid", "memberWorkloadUID": "member-uid", "observedGeneration": int64(1), "observedAt": now.Format(time.RFC3339), "readyRanks": int64(2), "worldSize": int64(2), "pods": pods}}}}
	if err := r.Update(ctx, rt); err != nil {
		t.Fatal(err)
	}
	req := newRestoreRequest()
	req.SetName("restore")
	req.SetNamespace("default")
	req.SetUID("restore-uid")
	req.SetGeneration(1)
	req.SetCreationTimestamp(metav1.NewTime(now))
	req.Object["spec"] = map[string]interface{}{"sourceCluster": "onpre1", "targetCluster": "aws", "workloadRef": map[string]interface{}{"uid": "workload-uid"}, "checkpointRef": map[string]interface{}{"checkpointID": "ckpt-1"}, "pods": []interface{}{map[string]interface{}{"sourceNode": "old-node"}}}
	req.Object["status"] = map[string]interface{}{"phase": "Verified", "observedGeneration": int64(1), "verification": map[string]interface{}{"requestUID": "restore-uid", "checkpointID": "ckpt-1", "verifiedAt": now.Format(time.RFC3339), "trainingRuntimeRef": map[string]interface{}{"name": rt.GetName(), "uid": "target-runtime-uid"}, "sourceCluster": "onpre1", "targetCluster": "aws", "sourceFence": map[string]interface{}{"fenced": true, "evidenceID": "fence-1", "observedAt": now.Format(time.RFC3339)}}}
	if err := r.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Minute).Format(time.RFC3339)
	if ok, err := r.verifiedTransition(ctx, policy, "aws", since); err != nil || !ok {
		t.Fatalf("current evidence rejected: %v %v", ok, err)
	}
	if ok, err := r.verifiedTransition(ctx, policy, "aws", now.Add(time.Minute).Format(time.RFC3339)); err != nil || ok {
		t.Fatalf("historical evidence accepted: %v %v", ok, err)
	}
	_ = unstructured.SetNestedField(req.Object, int64(0), "status", "observedGeneration")
	if err := r.Update(ctx, req); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.verifiedTransition(ctx, policy, "aws", since); err != nil || ok {
		t.Fatalf("stale generation accepted: %v %v", ok, err)
	}
}

func TestDiscoveryRejectsRecreatedWorkload(t *testing.T) {
	r, sts, _ := discoveryFixture(t, "aws")
	_ = discover(t, r, sts)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.SetUID("new-uid")
	if err := r.Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}); err == nil {
		t.Fatal("recreated workload adopted old policy")
	}
}
