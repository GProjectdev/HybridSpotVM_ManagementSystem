package management

import (
	"context"
	"reflect"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func userPolicy(sts *unstructured.Unstructured, cluster string) *unstructured.Unstructured {
	obj := p.NewObject("TrainingPolicy")
	obj.SetNamespace(sts.GetNamespace())
	obj.SetName("user-training")
	obj.SetUID("user-policy-uid")
	obj.SetGeneration(1)
	obj.Object["spec"] = map[string]interface{}{
		"workloadRef":   map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": sts.GetName(), "uid": string(sts.GetUID())},
		"sourceCluster": cluster, "targetWorkers": int64(2),
		"riskProfileRef": map[string]interface{}{"name": "train-risk"},
		"capacity":       map[string]interface{}{"aws": map[string]interface{}{"karmadaCluster": "aws", "vpcId": "vpc-user", "subnetId": "subnet-user", "securityGroupIds": []interface{}{"sg-user"}}},
	}
	return obj
}

func TestDiscoveryRejectsDuplicatePolicyAndForeignRuntime(t *testing.T) {
	for _, mode := range []string{"duplicate policy", "foreign runtime"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, sts, _ := discoveryFixture(t, "aws")
			obj := userPolicy(sts, "aws")
			if err := r.Create(ctx, obj); err != nil {
				t.Fatal(err)
			}
			if mode == "duplicate policy" {
				other := obj.DeepCopy()
				other.SetName("other-policy")
				other.SetUID("other-uid")
				other.SetResourceVersion("")
				if err := r.Create(ctx, other); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}); err == nil {
					t.Fatal("duplicate allocators accepted")
				}
				if _, err := automaticPlacement(ctx, r.Reader, obj); err == nil {
					t.Fatal("allocation gate accepted duplicate policies")
				}
			} else {
				foreign := p.NewObject("TrainingRuntime")
				foreign.SetNamespace(sts.GetNamespace())
				foreign.SetName(obj.GetName() + "-runtime")
				foreign.SetLabels(map[string]string{p.LabelPolicyUID: "other-owner"})
				if err := r.Create(ctx, foreign); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}); err != nil {
					t.Fatal(err)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatal(err)
				}
				if discoveryReady(obj) {
					t.Fatal("conflicting runtime accepted")
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil {
					t.Fatal(err)
				}
				if foreign.GetLabels()[p.LabelPolicyUID] != "other-owner" {
					t.Fatal("foreign runtime adopted")
				}
			}
		})
	}
}

func TestUserPolicyDiscoveryWatchAndGenerationGate(t *testing.T) {
	r, sts, _ := discoveryFixture(t, "aws")
	obj := discover(t, r, sts)
	requests := mapPolicyWorkload(context.Background(), obj)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(sts) {
		t.Fatal("policy event did not map to workload")
	}
	if !discoveryReady(obj) {
		t.Fatal("current discovery rejected")
	}
	obj.SetGeneration(obj.GetGeneration() + 1)
	if discoveryReady(obj) {
		t.Fatal("stale discovery accepted after user edit")
	}
}

func TestUserPolicyWithoutDiscoveryCannotAllocateOrCheckpoint(t *testing.T) {
	ctx := context.Background()
	r, sts, _ := discoveryFixture(t, "aws")
	obj := userPolicy(sts, "aws")
	now := time.Now().UTC()
	if err := r.Create(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, riskFixture(now)); err != nil {
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	pr := &PolicyReconciler{Client: r.Client, APIReader: r.Reader, Clock: func() time.Time { return now }}
	if _, err := pr.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	cp := &CheckpointReconciler{Client: r.Client}
	if _, err := cp.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"NodeProvision", "FluidCRMigration"} {
		list := p.NewList(kind)
		if err := r.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Fatalf("created %s before discovery", kind)
		}
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	if stringField(obj.Object, "status", "policy", "reason") != "placement_gate" {
		t.Fatal("allocation gate not applied")
	}
	if stringField(obj.Object, "status", "checkpoint", "reason") != "waiting_for_discovery" {
		t.Fatal("checkpoint gate not applied")
	}
}

func TestDiscoveryDoesNotCreatePolicyWithoutUserIntent(t *testing.T) {
	r, sts, _ := discoveryFixture(t, "aws")
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"TrainingPolicy", "TrainingRuntime", "NodeProvision"} {
		list := p.NewList(kind)
		if err := r.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Fatalf("created %s without user policy", kind)
		}
	}
}

func TestDiscoveryPreparesUserRuntimeWithoutEditingSpec(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default runtime", true: "explicit runtime"}[explicit], func(t *testing.T) {
			r, sts, _ := discoveryFixture(t, "aws")
			obj := userPolicy(sts, "aws")
			name := obj.GetName() + "-runtime"
			if explicit {
				name = "my-runtime"
				_ = unstructured.SetNestedField(obj.Object, name, "spec", "runtimeRef", "name")
			}
			before := obj.DeepCopy()
			ctx := context.Background()
			if err := r.Create(ctx, obj); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sts)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Object["spec"], obj.Object["spec"]) {
				t.Fatal("user spec changed")
			}
			runtime := p.NewObject("TrainingRuntime")
			if err := r.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: name}, runtime); err != nil {
				t.Fatal(err)
			}
			if runtime.GetLabels()[p.LabelPolicyUID] != string(obj.GetUID()) {
				t.Fatal("runtime not bound to policy UID")
			}
			if !boolField(obj.Object, "status", "discovery", "ready") {
				t.Fatal("user policy not discovered")
			}
		})
	}
}
