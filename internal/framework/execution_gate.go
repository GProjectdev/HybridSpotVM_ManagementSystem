package framework

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ExecutionPermit matches a concrete Pod incarnation. A name, Ready condition,
// lease, risk estimate or Cloud readiness alone cannot authorize training.
func ExecutionPermitted(run *unstructured.Unstructured, runUID, workUID, podName, podUID string) bool {
	if runUID == "" || workUID == "" || podUID == "" || podName == "" || string(run.GetUID()) != runUID || !run.GetDeletionTimestamp().IsZero() || boolAt(run, "spec", "suspend") {
		return false
	}
	return str(run, "status", "committedCluster") != "" && str(run, "status", "workloadUID") == workUID && str(run, "status", "executionPermit", "workloadUID") == workUID && str(run, "status", "executionPermit", "pods", podName) == podUID && str(run, "status", "executionPermit", "memberWorkloadUID") != "" && str(run, "status", "executionPermit", "cluster") == str(run, "status", "committedCluster")
}

// prepareExecutionGate must run before creating a placement policy. The shared
// credential is administrator-provisioned and only reads Run status in this
// validation namespace. It has no VM, Pod, checkpoint or status write rights.
func (r *RunReconciler) prepareExecutionGate(ctx context.Context, run, work *unstructured.Unstructured, s RunSpec) error {
	if !s.ExecutionGate {
		return nil
	}
	if !strings.Contains(r.GateImage, "@sha256:") || r.GateCredentialSecret == "" {
		return fmt.Errorf("pinned execution-gate image and read-only credential source required")
	}
	source := &corev1.Secret{}
	if err := r.Get(ctx, key(run.GetNamespace(), r.GateCredentialSecret), source); err != nil {
		return fmt.Errorf("execution-gate credential is unavailable")
	}
	if len(source.Data["kubeconfig"]) == 0 {
		return fmt.Errorf("execution-gate kubeconfig is absent")
	}
	secret := &corev1.Secret{}
	secret.Namespace = run.GetNamespace()
	secret.Name = run.GetName() + "-execution-access"
	err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	if apierrors.IsNotFound(err) {
		obj := object("v1", "Secret")
		obj.SetNamespace(secret.Namespace)
		obj.SetName(secret.Name)
		setOwner(obj, run)
		// Convert bytes via the typed API to preserve Secret base64 encoding.
		secret.Data = map[string][]byte{"kubeconfig": source.Data["kubeconfig"]}
		secret.OwnerReferences = obj.GetOwnerReferences()
		secret.Type = corev1.SecretTypeOpaque
		if err = r.Create(ctx, secret); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		u := object("v1", "Secret")
		u.SetName(secret.Name)
		u.SetOwnerReferences(secret.OwnerReferences)
		if !owned(u, run) {
			return fmt.Errorf("execution-gate Secret ownership conflict")
		}
		if !reflect.DeepEqual(secret.Data["kubeconfig"], source.Data["kubeconfig"]) {
			base := secret.DeepCopy()
			secret.Data = map[string][]byte{"kubeconfig": source.Data["kubeconfig"]}
			if err = r.Patch(ctx, secret, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	}
	u := object("v1", "Secret")
	u.SetName(secret.Name)
	u.SetNamespace(secret.Namespace)
	if err := ensureGateDependencyPlacement(ctx, r.Client, run, u, s); err != nil {
		return err
	}
	gate := map[string]interface{}{"name": "training-execution-permit", "image": r.GateImage, "command": []interface{}{"/manager"}, "args": []interface{}{"--kubeconfig=/training-access/kubeconfig", "--namespace=" + run.GetNamespace(), "--run=" + run.GetName(), "--run-uid=" + string(run.GetUID()), "--workload-uid=" + string(work.GetUID())}, "env": []interface{}{map[string]interface{}{"name": "POD_NAME", "valueFrom": map[string]interface{}{"fieldRef": map[string]interface{}{"fieldPath": "metadata.name"}}}, map[string]interface{}{"name": "POD_UID", "valueFrom": map[string]interface{}{"fieldRef": map[string]interface{}{"fieldPath": "metadata.uid"}}}}, "resources": map[string]interface{}{"requests": map[string]interface{}{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]interface{}{"cpu": "100m", "memory": "64Mi"}}, "securityContext": map[string]interface{}{"runAsNonRoot": true, "runAsUser": int64(65532), "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true}, "volumeMounts": []interface{}{map[string]interface{}{"name": "training-execution-access", "mountPath": "/training-access", "readOnly": true}}}
	init, _, _ := unstructured.NestedSlice(work.Object, "spec", "template", "spec", "initContainers")
	for _, raw := range init {
		if m, ok := raw.(map[string]interface{}); ok && m["name"] == "training-execution-permit" {
			if !containsSpec(m, gate) {
				return fmt.Errorf("execution-gate template conflict")
			}
			return nil
		}
	}
	// Injecting into an already propagated workload cannot protect old Pods.
	bindings, err := listItems(ctx, r.Client, "work.karmada.io/v1alpha2", "ResourceBinding", run.GetNamespace())
	if err != nil {
		return err
	}
	for i := range bindings {
		if str(&bindings[i], "spec", "resource", "uid") == string(work.GetUID()) {
			return fmt.Errorf("cannot retrofit execution gate after workload propagation")
		}
	}
	volumes, _, _ := unstructured.NestedSlice(work.Object, "spec", "template", "spec", "volumes")
	for _, raw := range volumes {
		if m, ok := raw.(map[string]interface{}); ok && m["name"] == "training-execution-access" {
			return fmt.Errorf("execution credential volume name conflict")
		}
	}
	volumes = append(volumes, map[string]interface{}{"name": "training-execution-access", "secret": map[string]interface{}{"secretName": secret.Name, "defaultMode": int64(292)}})
	base := work.DeepCopy()
	_ = unstructured.SetNestedSlice(work.Object, append([]interface{}{gate}, init...), "spec", "template", "spec", "initContainers")
	_ = unstructured.SetNestedSlice(work.Object, volumes, "spec", "template", "spec", "volumes")
	return r.Patch(ctx, work, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// initialPermit is immutable after grant. A replacement Pod requires a verified
// transition, never an automatic rewrite of the allowed Pod UIDs.
func initialPermit(status map[string]interface{}, work *unstructured.Unstructured, o WorkloadObservation, now string) error {
	if !o.AllPodsScheduled || o.MemberWorkloadUID == "" {
		return nil
	}
	pods := map[string]interface{}{}
	for name, uid := range o.ScheduledPods {
		pods[name] = uid
	}
	if existing, ok := status["executionPermit"].(map[string]interface{}); ok {
		if existing["cluster"] != o.SelectedCluster || existing["memberWorkloadUID"] != o.MemberWorkloadUID || !reflect.DeepEqual(existing["pods"], pods) {
			return fmt.Errorf("Pod incarnation changed; verified transition required before new execution grant")
		}
		return nil
	}
	status["executionPermit"] = map[string]interface{}{"workloadUID": string(work.GetUID()), "memberWorkloadUID": o.MemberWorkloadUID, "cluster": o.SelectedCluster, "pods": pods, "issuedAt": now}
	return nil
}
