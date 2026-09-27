package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"
)

const autoLabel = "training.dcnlab.com/automatic"
const defaultsName = "automatic-policy-defaults"

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

func autoName(name string) string {
	h := sha256.Sum256([]byte(name))
	if len(name) > 35 {
		name = name[:35]
	}
	return name + "-auto-" + hex.EncodeToString(h[:4])
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
	policy := p.NewObject("TrainingPolicy")
	key := types.NamespacedName{Namespace: sts.GetNamespace(), Name: autoName(sts.GetName())}
	err := r.Reader.Get(ctx, key, policy)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return again, err
	}
	report := func(reason string) (ctrl.Result, error) {
		if !exists {
			ctrl.LoggerFrom(ctx).Info("workload discovery waiting", "workload", req.NamespacedName, "reason", reason)
			return again, nil
		}
		return again, patchStatusSubtree(ctx, r.Client, policy, "discovery", map[string]interface{}{"ready": false, "reason": reason, "observedAt": time.Now().UTC().Format(time.RFC3339)})
	}
	if exists && (policy.GetLabels()[autoLabel] != "true" || stringField(policy.Object, "spec", "workloadRef", "uid") != string(sts.GetUID())) {
		return again, fmt.Errorf("automatic policy name collision; refusing adoption or recreated workload")
	}
	b, target, err := selectedBinding(ctx, r.Reader, sts)
	if err != nil {
		return report(err.Error())
	}
	if !exists {
		// A manually configured policy remains authoritative; never create a second allocator.
		policies := p.NewList("TrainingPolicy")
		if err := r.Reader.List(ctx, policies, client.InNamespace(sts.GetNamespace())); err != nil {
			return again, err
		}
		for _, other := range policies.Items {
			if stringField(other.Object, "spec", "workloadRef", "name") == sts.GetName() {
				return report("existing workload policy prevents automatic adoption")
			}
		}
		cm := &corev1.ConfigMap{}
		if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: "hybridspot-system", Name: defaultsName}, cm); err != nil {
			return report("configure hybridspot-system/" + defaultsName)
		}
		var spec map[string]interface{}
		raw, decodeErr := yaml.YAMLToJSON([]byte(cm.Data["spec.yaml"]))
		if decodeErr != nil {
			return report("invalid defaults YAML")
		}
		if err := utiljson.Unmarshal(raw, &spec); err != nil || spec == nil {
			return report("invalid defaults YAML")
		}
		for k := range spec {
			switch k {
			case "capacity", "policy", "checkpoint", "riskProfileRef", "replacement":
			default:
				return report("unsupported defaults key: " + k)
			}
		}
		if stringField(spec, "riskProfileRef", "name") == "" || stringField(spec, "capacity", "aws", "karmadaCluster") == "" {
			return report("riskProfileRef and AWS cluster defaults required")
		}
		if strings.Contains(string(raw), "__REPLACE_") {
			return report("replace all AWS defaults placeholders before enabling")
		}
		for _, field := range []string{"region", "instanceType", "ami", "subnetId", "vpcId"} {
			if stringField(spec, "capacity", "aws", field) == "" {
				return report("missing AWS default: " + field)
			}
		}
		if stringField(spec, "capacity", "aws", "credentialsRef", "name") == "" {
			return report("AWS credentialsRef.name is required")
		}
		replicas, ok, _ := unstructured.NestedInt64(sts.Object, "spec", "replicas")
		if !ok {
			replicas = 1
		}
		spec["workloadRef"] = map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": sts.GetName(), "uid": string(sts.GetUID())}
		spec["sourceCluster"] = target
		spec["targetWorkers"] = replicas
		spec["expectedWorldSize"] = replicas
		spec["runtimeRef"] = map[string]interface{}{"name": runtimeName(key.Name, target)}
		policy.SetName(key.Name)
		policy.SetNamespace(key.Namespace)
		policy.SetLabels(map[string]string{autoLabel: "true"})
		policy.Object["spec"] = spec
		if err := r.Create(ctx, policy); err != nil {
			return again, err
		}
		return again, nil
	}
	input := p.ReadPolicySpec(policy)
	replicas, ok, _ := unstructured.NestedInt64(sts.Object, "spec", "replicas")
	if !ok {
		replicas = 1
	}
	if replicas != input.TargetWorkers {
		return report("replica change requires an explicit capacity transition")
	}
	// The origin UID label is required by the existing member-side runtime verifier.
	if sts.GetLabels()["training.dcnlab.com/workload-uid"] != string(sts.GetUID()) {
		before := sts.DeepCopy()
		labels := sts.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["training.dcnlab.com/workload-uid"] = string(sts.GetUID())
		sts.SetLabels(labels)
		if err := r.Patch(ctx, sts, client.MergeFrom(before)); err != nil {
			return again, err
		}
	}
	if err := r.ensureRuntime(ctx, policy, sts, input.SourceCluster); err != nil {
		return report(err.Error())
	}
	phase := "Stable"
	status := map[string]interface{}{"ready": true, "phase": phase, "sourceCluster": input.SourceCluster, "targetCluster": target, "bindingName": b.GetName(), "bindingUID": string(b.GetUID()), "observedAt": time.Now().UTC().Format(time.RFC3339), "transitionStartedAt": nil, "dispatchSuspended": nil}
	if target != input.SourceCluster {
		status["phase"] = "MigrationRequired"
		// Keep the first observed transition timestamp stable so old restore evidence cannot match.
		started := stringField(policy.Object, "status", "discovery", "transitionStartedAt")
		if stringField(policy.Object, "status", "discovery", "targetCluster") != target || started == "" {
			started = time.Now().UTC().Format(time.RFC3339)
		}
		status["transitionStartedAt"] = started
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
			before := policy.DeepCopy()
			_ = unstructured.SetNestedField(policy.Object, target, "spec", "sourceCluster")
			_ = unstructured.SetNestedField(policy.Object, runtimeName(policy.GetName(), target), "spec", "runtimeRef", "name")
			if err := r.Patch(ctx, policy, client.MergeFrom(before)); err != nil {
				return again, err
			}
			return again, nil
		}
	}
	return again, patchStatusSubtree(ctx, r.Client, policy, "discovery", status)
}

func runtimeName(policy, cluster string) string {
	h := sha256.Sum256([]byte(cluster))
	return policy + "-rt-" + hex.EncodeToString(h[:3])
}

func (r *DiscoveryReconciler) ensureRuntime(ctx context.Context, policy, sts *unstructured.Unstructured, cluster string) error {
	input := p.ReadPolicySpec(policy)
	obj := p.NewObject("TrainingRuntime")
	obj.SetName(runtimeName(policy.GetName(), cluster))
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
	if _, inflight, _, _, err := cp.checkpointState(ctx, p.ReadPolicySpec(policy)); err != nil {
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
	input := p.ReadPolicySpec(policy)
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

// Automatic policies recheck live intent before creating any cloud capacity.
func automaticPlacement(ctx context.Context, r client.Reader, policy *unstructured.Unstructured) (string, error) {
	sts := p.NewObject("StatefulSet")
	if err := r.Get(ctx, types.NamespacedName{Namespace: policy.GetNamespace(), Name: stringField(policy.Object, "spec", "workloadRef", "name")}, sts); err != nil {
		return "", err
	}
	if string(sts.GetUID()) != stringField(policy.Object, "spec", "workloadRef", "uid") || !sts.GetDeletionTimestamp().IsZero() {
		return "", fmt.Errorf("workload UID changed or deleting")
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
	if target != stringField(policy.Object, "spec", "sourceCluster") {
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
			match := kind == "binding" || (kind == "risk" && stringField(item.Object, "spec", "riskProfileRef", "name") == obj.GetName()) || (kind == "runtime" && stringField(item.Object, "spec", "runtimeRef", "name") == obj.GetName())
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
		Watches(bindingObject(), handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			b, ok := obj.(*unstructured.Unstructured)
			if !ok || stringField(b.Object, "spec", "resource", "kind") != "StatefulSet" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: b.GetNamespace(), Name: stringField(b.Object, "spec", "resource", "name")}}}
		})).Complete(r)
}
