package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type DiscoveryReconciler struct {
	client.Client
	Reader client.Reader
}

func bindingObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: "work.karmada.io", Version: "v1alpha2", Kind: "ResourceBinding"})
	return o
}

// UID-bound selection avoids treating a recreated workload's old binding as intent.
func selectedBinding(ctx context.Context, r client.Reader, sts *unstructured.Unstructured) (*unstructured.Unstructured, string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(bindingObject().GroupVersionKind().GroupVersion().WithKind("ResourceBindingList"))
	if err := r.List(ctx, list, client.InNamespace(sts.GetNamespace())); err != nil {
		return nil, "", err
	}
	var found *unstructured.Unstructured
	for i := range list.Items {
		b := &list.Items[i]
		if stringField(b.Object, "spec", "resource", "apiVersion") != "apps/v1" || stringField(b.Object, "spec", "resource", "kind") != "StatefulSet" || stringField(b.Object, "spec", "resource", "name") != sts.GetName() || stringField(b.Object, "spec", "resource", "namespace") != sts.GetNamespace() {
			continue
		}
		if stringField(b.Object, "spec", "resource", "uid") != string(sts.GetUID()) || !b.GetDeletionTimestamp().IsZero() {
			return nil, "", fmt.Errorf("binding UID is stale or binding is deleting")
		}
		if found != nil {
			return nil, "", fmt.Errorf("multiple bindings for workload")
		}
		found = b
	}
	if found == nil {
		return nil, "", fmt.Errorf("waiting for UID-bound ResourceBinding")
	}
	clusters, _, _ := unstructured.NestedSlice(found.Object, "spec", "clusters")
	if len(clusters) != 1 {
		return found, "", fmt.Errorf("exactly one target cluster required; split/duplicated placement is unsupported")
	}
	cluster, ok := clusters[0].(map[string]interface{})
	if !ok {
		return found, "", fmt.Errorf("invalid target")
	}
	target, _ := cluster["name"].(string)
	replicas, ok, err := unstructured.NestedInt64(sts.Object, "spec", "replicas")
	if err != nil {
		return found, "", err
	}
	if !ok {
		replicas = 1
	}
	if target == "" || replicas <= 0 {
		return found, "", fmt.Errorf("target and positive replicas required")
	}
	if assigned, exists := cluster["replicas"]; exists && assigned != replicas {
		return found, "", fmt.Errorf("partial replica assignment is unsupported")
	}
	return found, target, nil
}

// A workload has one allocator, explicitly supplied by its owner.
func workloadPolicy(ctx context.Context, reader client.Reader, sts *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	policies := p.NewList("TrainingPolicy")
	if err := reader.List(ctx, policies, client.InNamespace(sts.GetNamespace())); err != nil {
		return nil, err
	}
	var found *unstructured.Unstructured
	for i := range policies.Items {
		obj := &policies.Items[i]
		if stringField(obj.Object, "spec", "workloadRef", "apiVersion") != "apps/v1" ||
			stringField(obj.Object, "spec", "workloadRef", "kind") != "StatefulSet" ||
			stringField(obj.Object, "spec", "workloadRef", "name") != sts.GetName() {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple TrainingPolicies reference workload; refusing multiple allocators")
		}
		found = obj
	}
	if found != nil && (found.GetUID() == "" || !found.GetDeletionTimestamp().IsZero() ||
		stringField(found.Object, "spec", "workloadRef", "uid") != string(sts.GetUID())) {
		return nil, fmt.Errorf("policy/workload identity is stale or policy is deleting")
	}
	return found, nil
}

func (r *DiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: 15 * time.Second}
	sts := p.NewObject("StatefulSet")
	if err := r.Reader.Get(ctx, req.NamespacedName, sts); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sts.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	policy, err := workloadPolicy(ctx, r.Reader, sts)
	if err != nil {
		return again, err
	}
	if policy == nil {
		return ctrl.Result{}, nil
	}
	report := func(reason string) (ctrl.Result, error) {
		return again, patchStatusSubtree(ctx, r.Client, policy, "discovery", map[string]interface{}{"ready": false, "reason": reason, "observedAt": time.Now().UTC().Format(time.RFC3339)})
	}
	// Stamp identity while scaled to zero too, before the first Pods are created.
	templateUID, _, _ := unstructured.NestedString(sts.Object, "spec", "template", "metadata", "labels", "training.dcnlab.com/workload-uid")
	if sts.GetLabels()["training.dcnlab.com/workload-uid"] != string(sts.GetUID()) || templateUID != string(sts.GetUID()) {
		before := sts.DeepCopy()
		labels := sts.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["training.dcnlab.com/workload-uid"] = string(sts.GetUID())
		sts.SetLabels(labels)
		if templateUID != string(sts.GetUID()) {
			// Preserve running survivors when repairing the template identity.
			if err := unstructured.SetNestedField(sts.Object, "OnDelete", "spec", "updateStrategy", "type"); err != nil {
				return again, err
			}
			unstructured.RemoveNestedField(sts.Object, "spec", "updateStrategy", "rollingUpdate")
			if err := unstructured.SetNestedField(sts.Object, string(sts.GetUID()), "spec", "template", "metadata", "labels", "training.dcnlab.com/workload-uid"); err != nil {
				return again, err
			}
		}
		if err := r.Patch(ctx, sts, client.MergeFrom(before)); err != nil {
			return again, err
		}
	}
	b, target, err := selectedBinding(ctx, r.Reader, sts)
	if err != nil {
		return report(err.Error())
	}
	input := p.ReadPolicyInput(policy)
	replicas, ok, _ := unstructured.NestedInt64(sts.Object, "spec", "replicas")
	if !ok {
		replicas = 1
	}
	if replicas != input.TargetWorkers {
		return report("replica change requires an explicit capacity transition")
	}
	if err := r.ensureRuntime(ctx, policy, sts, input.SourceCluster); err != nil {
		return report(err.Error())
	}
	phase := "Stable"
	status := map[string]interface{}{"ready": true, "phase": phase, "observedGeneration": policy.GetGeneration(), "workloadUID": string(sts.GetUID()), "runtimeRef": map[string]interface{}{"name": input.RuntimeRefName}, "reason": nil, "sourceCluster": input.SourceCluster, "targetCluster": target, "bindingName": b.GetName(), "bindingUID": string(b.GetUID()), "observedAt": time.Now().UTC().Format(time.RFC3339), "transitionStartedAt": nil, "dispatchSuspended": nil}
	if target != input.SourceCluster {
		status["phase"] = "MigrationRequired"
		// Keep the first observed transition timestamp stable so old restore evidence cannot match.
		started := stringField(policy.Object, "status", "discovery", "transitionStartedAt")
		if stringField(policy.Object, "status", "discovery", "targetCluster") != target || started == "" {
			started = time.Now().UTC().Format(time.RFC3339)
		}
		status["transitionStartedAt"] = started
		if name := b.GetAnnotations()[placementRequestAnnotation]; name != "" {
			request := newRestoreRequest()
			if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: policy.GetNamespace(), Name: name}, request); err != nil {
				return again, err
			}
			if string(request.GetUID()) != b.GetAnnotations()[placementRequestUIDAnnotation] || request.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) || stringField(request.Object, "spec", "workloadRef", "uid") != string(sts.GetUID()) || stringField(request.Object, "spec", "targetCluster") != target {
				return report("bound placement restore identity mismatch")
			}
			status["ready"] = false
			status["phase"] = "Restoring"
			status["transitionStartedAt"] = request.GetCreationTimestamp().UTC().Format(time.RFC3339)
			if validateGroupVerified(request) == nil {
				return again, r.recordVerifiedPlacement(ctx, policy, target)
			}
			return again, patchStatusSubtree(ctx, r.Client, policy, "discovery", status)
		}
		suspended, _, _ := unstructured.NestedBool(b.Object, "spec", "suspension", "dispatching")
		status["dispatchSuspended"] = suspended
		if !suspended {
			status["phase"] = "UnsafePlacementChange"
		}
		if err := r.ensureRuntime(ctx, policy, sts, target); err != nil {
			return report(err.Error())
		}
		verified, err := r.verifiedTransition(ctx, policy, target, started)
		if err != nil {
			return again, err
		}
		if verified && !suspended {
			return again, r.recordVerifiedPlacement(ctx, policy, target)
		}
	}
	return again, patchStatusSubtree(ctx, r.Client, policy, "discovery", status)
}

// Placement is controller-owned observation; the user's initial intent is immutable to us.
func (r *DiscoveryReconciler) recordVerifiedPlacement(ctx context.Context, policy *unstructured.Unstructured, target string) error {
	input := p.ReadPolicySpec(policy)
	before := policy.DeepCopy()
	_ = unstructured.SetNestedMap(policy.Object, map[string]interface{}{
		"policyUID": string(policy.GetUID()), "workloadUID": string(input.WorkloadRef.UID),
		"initialSourceCluster": input.SourceCluster, "initialRuntimeName": input.RuntimeRefName,
		"activeCluster": target, "runtimeRef": map[string]interface{}{"name": runtimeName(policy.GetName(), target)},
		"verified": true, "verifiedAt": time.Now().UTC().Format(time.RFC3339),
	}, "status", "placement")
	// A concurrent spec update must force verification against the new intent.
	return r.Status().Patch(ctx, policy, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func runtimeName(policy, cluster string) string {
	h := sha256.Sum256([]byte(cluster))
	return policy + "-rt-" + hex.EncodeToString(h[:3])
}

func discoveryReady(policy *unstructured.Unstructured) bool {
	return boolField(policy.Object, "status", "discovery", "ready") &&
		intField(policy.Object, "status", "discovery", "observedGeneration") == policy.GetGeneration() &&
		stringField(policy.Object, "status", "discovery", "workloadUID") == stringField(policy.Object, "spec", "workloadRef", "uid")
}

func (r *DiscoveryReconciler) ensureRuntime(ctx context.Context, policy, sts *unstructured.Unstructured, cluster string) error {
	input := p.ReadPolicyInput(policy)
	obj := p.NewObject("TrainingRuntime")
	name := runtimeName(policy.GetName(), cluster)
	if cluster == input.SourceCluster {
		name = input.RuntimeRefName
	}
	obj.SetName(name)
	obj.SetNamespace(policy.GetNamespace())
	obj.SetLabels(map[string]string{p.LabelPolicyUID: string(policy.GetUID())})
	containers, _, _ := unstructured.NestedSlice(sts.Object, "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		return fmt.Errorf("automatic runtime requires one application container")
	}
	container, ok := containers[0].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid container")
	}
	obj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": sts.GetName(), "uid": string(sts.GetUID())}, "sourceCluster": cluster, "expectedWorldSize": input.TargetWorkers, "port": int64(8298), "container": container["name"]}
	existing := p.NewObject("TrainingRuntime")
	err := r.Reader.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		if err = r.Create(ctx, obj); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if existing.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) || !reflect.DeepEqual(existing.Object["spec"], obj.Object["spec"]) {
		return fmt.Errorf("runtime identity/spec conflict")
	}
	pr := &PolicyReconciler{Client: r.Client, APIReader: r.Reader}
	return pr.createIfMissing(ctx, p.NewPropagationPolicyFor(input, obj, cluster))
}

func (r *DiscoveryReconciler) verifiedTransition(ctx context.Context, policy *unstructured.Unstructured, target, started string) (bool, error) {
	cp := &CheckpointReconciler{Client: r.Client}
	if _, inflight, _, _, err := cp.checkpointState(ctx, p.ReadPolicyInput(policy)); err != nil {
		return false, err
	} else if inflight {
		return false, nil
	}
	since, err := time.Parse(time.RFC3339, started)
	if err != nil {
		return false, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(restoreRequestGVK.GroupVersion().WithKind("RestoreRequestList"))
	if err := r.Reader.List(ctx, list, client.InNamespace(policy.GetNamespace())); err != nil {
		return false, err
	}
	input := p.ReadPolicyInput(policy)
	for _, req := range list.Items {
		if req.GetCreationTimestamp().Time.Before(since) || req.GetUID() == "" || req.GetGeneration() < 1 {
			continue
		}
		runtime := p.NewObject("TrainingRuntime")
		name := runtimeName(policy.GetName(), target)
		if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: policy.GetNamespace(), Name: name}, runtime); err != nil {
			return false, err
		}
		spec := recoverySpec{RestoreRequestName: req.GetName(), RestoreRequestUID: string(req.GetUID()), RestoreRequestGeneration: req.GetGeneration(), RequestUID: string(req.GetUID()), SourceCluster: input.SourceCluster, TargetCluster: target, WorkloadUID: string(input.WorkloadRef.UID), CheckpointID: stringField(req.Object, "spec", "checkpointRef", "checkpointID"), TrainingRuntimeName: name, TrainingRuntimeUID: string(runtime.GetUID())}
		rr := &RecoveryReconciler{Client: r.Client, APIReader: r.Reader}
		if _, err := rr.verifyRestoreRequest(ctx, policy.GetNamespace(), spec); err != nil {
			continue
		}
		if p.RuntimeReadyForCheckpoint(input, p.ReadRuntimeStatus(runtime, target), time.Now().UTC()) {
			return true, nil
		}
	}
	return false, nil
}

// Every policy rechecks live intent before creating cloud capacity.
func automaticPlacement(ctx context.Context, r client.Reader, policy *unstructured.Unstructured) (string, error) {
	sts := p.NewObject("StatefulSet")
	if err := r.Get(ctx, types.NamespacedName{Namespace: policy.GetNamespace(), Name: stringField(policy.Object, "spec", "workloadRef", "name")}, sts); err != nil {
		return "", err
	}
	if string(sts.GetUID()) != stringField(policy.Object, "spec", "workloadRef", "uid") || !sts.GetDeletionTimestamp().IsZero() {
		return "", fmt.Errorf("workload UID changed or deleting")
	}
	owner, err := workloadPolicy(ctx, r, sts)
	if err != nil {
		return "", err
	}
	if owner == nil || owner.GetUID() != policy.GetUID() {
		return "", fmt.Errorf("policy is not the unique workload owner")
	}
	b, target, err := selectedBinding(ctx, r, sts)
	if err != nil {
		return "", err
	}
	replicas, ok, _ := unstructured.NestedInt64(sts.Object, "spec", "replicas")
	if !ok {
		replicas = 1
	}
	if replicas != intField(policy.Object, "spec", "targetWorkers") {
		return "", fmt.Errorf("replica change requires explicit operation")
	}
	if target != p.ReadPolicyInput(policy).SourceCluster {
		suspended, _, _ := unstructured.NestedBool(b.Object, "spec", "suspension", "dispatching")
		if !suspended {
			return "", fmt.Errorf("migration target requires pre-established dispatch suspension")
		}
	}
	return target, nil
}

func mapPolicies(c client.Client, kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		list := p.NewList("TrainingPolicy")
		if err := c.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "map policy dependencies")
			return nil
		}
		var out []reconcile.Request
		for _, item := range list.Items {
			input := p.ReadPolicyInput(&item)
			match := kind == "binding" || (kind == "risk" && input.RiskProfileName == obj.GetName()) || (kind == "runtime" && input.RuntimeRefName == obj.GetName())
			if kind == "node" || kind == "replacement" {
				match = item.GetUID() != "" && obj.GetLabels()[p.LabelPolicyUID] == string(item.GetUID())
			}
			if match {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&item)})
			}
		}
		return out
	}
}

func (r *DiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("statefulset-policy-discovery").
		For(p.NewObject("StatefulSet")).
		Watches(p.NewObject("TrainingPolicy"), handler.EnqueueRequestsFromMapFunc(mapPolicyWorkload), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(bindingObject(), handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			b, ok := obj.(*unstructured.Unstructured)
			if !ok || stringField(b.Object, "spec", "resource", "kind") != "StatefulSet" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: b.GetNamespace(), Name: stringField(b.Object, "spec", "resource", "name")}}}
		})).Complete(r)
}

func mapPolicyWorkload(_ context.Context, obj client.Object) []reconcile.Request {
	policy, ok := obj.(*unstructured.Unstructured)
	if !ok || stringField(policy.Object, "spec", "workloadRef", "apiVersion") != "apps/v1" ||
		stringField(policy.Object, "spec", "workloadRef", "kind") != "StatefulSet" {
		return nil
	}
	name := stringField(policy.Object, "spec", "workloadRef", "name")
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}
