package management

import (
	"context"
	"fmt"
	"reflect"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type GroupPlacementReconciler struct {
	client.Client
	Reader         client.Reader
	ExistingNodes  func(context.Context, p.PolicyInput) (map[string]string, error)
	ExistingClaims func(context.Context, p.PolicyInput, *unstructured.Unstructured, *unstructured.Unstructured) (bool, error)
}

// Explicit fault injection starts UID-bound source fencing before the Pod is
// removed. It does not invent a Karmada scheduling decision.
const applicationFailoverTrigger = "training.dcnlab.com/application-failover-trigger"

func (r *GroupPlacementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("group-placement-management").For(bindingObject()).Complete(r)
}
func (r *GroupPlacementReconciler) Reconcile(ctx context.Context, key ctrl.Request) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: 5 * time.Second}
	binding := bindingObject()
	if err := r.Reader.Get(ctx, key.NamespacedName, binding); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !binding.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	pending := binding.GetAnnotations()[pendingPlacementAnnotation]
	trigger := binding.GetAnnotations()[applicationFailoverTrigger]
	if pending == "" && trigger == "" {
		return ctrl.Result{}, nil
	}
	var clusters []interface{}
	if pending != "" {
		if err := utiljson.Unmarshal([]byte(pending), &clusters); err != nil {
			return again, err
		}
	} else {
		clusters = []interface{}{map[string]interface{}{"name": ""}}
	}
	if len(clusters) != 1 {
		return again, fmt.Errorf("single target placement required")
	}
	cluster, ok := clusters[0].(map[string]interface{})
	if !ok {
		return again, fmt.Errorf("invalid target placement")
	}
	target := stringField(cluster, "name")
	policies := p.NewList("TrainingPolicy")
	if err := r.Reader.List(ctx, policies, client.InNamespace(key.Namespace)); err != nil {
		return again, err
	}
	var policy *unstructured.Unstructured
	for i := range policies.Items {
		candidate := &policies.Items[i]
		if stringField(candidate.Object, "spec", "workloadRef", "uid") == stringField(binding.Object, "spec", "resource", "uid") {
			if policy != nil {
				return again, fmt.Errorf("ambiguous placement policy")
			}
			policy = candidate
		}
	}
	if policy == nil {
		return again, fmt.Errorf("placement policy not found")
	}
	if !policy.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	input := p.ReadPolicyInput(policy)
	if err := validatePolicyInput(input); err != nil {
		return again, err
	}
	if trigger != "" {
		if trigger != string(binding.GetUID()) || binding.GetUID() == "" || stringField(policy.Object, "spec", "capacity", "mode") != "Existing" {
			return again, fmt.Errorf("application failover trigger requires existing capacity and the current binding UID")
		}
		if pending == "" {
			target = input.Capacity.AWSCluster
			// After release, the trigger must not start a second operation.
			if binding.GetAnnotations()[placementRequestUIDAnnotation] != "" && input.SourceCluster == target {
				return ctrl.Result{}, nil
			}
		}
	}
	if target != input.Capacity.AWSCluster || target == input.SourceCluster {
		return again, fmt.Errorf("group placement requires source to configured AWS cluster")
	}
	if binding.GetAnnotations()[recoverSelectedPlacement] != "" {
		next, e := recoverSelectedBinding(ctx, r.Reader, policy, binding)
		if e != nil {
			return again, e
		}
		return again, r.Patch(ctx, next, client.MergeFromWithOptions(binding.DeepCopy(), client.MergeFromWithOptimisticLock{}))
	}
	current, sourceErr := applicationFailoverSourcePlacement(binding)
	if sourceErr != nil {
		return again, sourceErr
	}
	if len(current) != 1 {
		return again, fmt.Errorf("exactly one source placement required")
	}
	currentCluster, valid := current[0].(map[string]interface{})
	if !valid || stringField(currentCluster, "name") != input.SourceCluster {
		return again, fmt.Errorf("source placement changed during operation")
	}
	if replicas, present := cluster["replicas"]; present && replicas != input.TargetWorkers {
		return again, fmt.Errorf("full world placement required")
	}
	producer := &PolicyReconciler{Client: r.Client, APIReader: r.Reader}
	operation := string(binding.GetUID())
	if operation == "" {
		return again, fmt.Errorf("binding UID required")
	}
	name := groupPlacementRequestName(input.PolicyName, operation, target)
	request := newRestoreRequest()
	err := r.Reader.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: name}, request)
	if apierrors.IsNotFound(err) {
		if input.Suspended {
			return again, nil
		}
		round, e := selectGroupCheckpoint(ctx, r.Reader, input)
		if e != nil {
			return again, e
		}
		source, e := producer.groupSourcePods(ctx, input)
		if e != nil {
			return again, e
		}
		pvc, root, e := producer.groupCheckpointVolume(ctx, input)
		if e != nil {
			return again, e
		}
		nodes := map[string]string{}
		if input.Capacity.Mode == "Existing" {
			if r.ExistingNodes == nil {
				return again, fmt.Errorf("existing-capacity member reader is not configured")
			}
			nodes, e = r.ExistingNodes(ctx, input)
			if e != nil {
				return again, e
			}
		}
		if ready, e := producer.quiesceGroup(ctx, policy, operation); e != nil {
			return again, e
		} else if !ready {
			return again, nil
		}
		if input.Capacity.Mode != "Existing" {
			for rank := int64(0); rank < input.TargetWorkers; rank++ {
				// Initial cross-cluster recovery uses stable OnDemand capacity. The policy
				// may subsequently replace it according to its risk decision.
				desired := p.NewNodeProvision(input, rank, "OnDemand")
				if e := producer.createIfMissing(ctx, desired); e != nil {
					return again, e
				}
				if e := producer.createIfMissing(ctx, p.NewPropagationPolicyFor(input, desired, target)); e != nil {
					return again, e
				}
				np := p.NewObject("NodeProvision")
				if e := r.Reader.Get(ctx, client.ObjectKeyFromObject(desired), np); e != nil {
					return again, e
				}
				if stringField(np.Object, "status", "phase") != "Ready" || stringField(np.Object, "status", "nodeName") == "" {
					return again, nil
				}
				nodes[fmt.Sprintf("%s-%d", input.WorkloadRef.Name, rank)] = stringField(np.Object, "status", "nodeName")
			}
		}
		sts := p.NewObject("StatefulSet")
		if e := r.Reader.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: input.WorkloadRef.Name}, sts); e != nil {
			return again, e
		}
		discovery := &DiscoveryReconciler{Client: r.Client, Reader: r.Reader}
		if e := discovery.ensureRuntime(ctx, policy, sts, target); e != nil {
			return again, e
		}
		if _, inflight, _, _, e := (&CheckpointReconciler{Client: r.Client}).checkpointState(ctx, input); e != nil {
			return again, e
		} else if inflight {
			return again, nil
		}
		request, e = producer.newGroupRequest(ctx, input, name, operation, target, round, source, pvc, root, nodes)
		if e != nil {
			return again, e
		}
		if e := r.Create(ctx, request); e != nil {
			return again, e
		}
		return again, nil
	} else if err != nil {
		return again, err
	}
	if request.GetUID() == "" || request.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) || stringField(request.Object, "spec", "groupRestore", "operationUID") != operation || stringField(request.Object, "spec", "targetCluster") != target {
		return again, fmt.Errorf("placement request collision")
	}
	a := binding.GetAnnotations()
	if a[placementRequestAnnotation] != request.GetName() || a[placementRequestUIDAnnotation] != string(request.GetUID()) {
		if a[placementRequestAnnotation] != "" {
			return again, fmt.Errorf("placement request changed")
		}
		before := binding.DeepCopy()
		a[placementRequestAnnotation] = request.GetName()
		a[placementRequestUIDAnnotation] = string(request.GetUID())
		binding.SetAnnotations(a)
		return again, r.Patch(ctx, binding, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	// The target cannot prepare its group checkpoint until its generated PVC
	// exists. Source fencing, not target Prepared, gates volume migration.
	if e := validateGroupSourceFence(ctx, r.Reader, request); e != nil {
		return again, nil
	}
	if ready, e := r.ensureGroupVolumes(ctx, binding, request); e != nil {
		return again, e
	} else if !ready {
		return again, nil
	}
	if e := validateGroupVolumes(ctx, r.Reader, binding, request); e != nil {
		return again, nil
	}
	if input.Capacity.Mode == p.CapacityModeExisting {
		if r.ExistingClaims == nil {
			return again, fmt.Errorf("existing-capacity claim preparer is not configured")
		}
		if ready, e := r.ExistingClaims(ctx, input, binding, request); e != nil {
			return again, e
		} else if !ready {
			return again, nil
		}
	}
	if pending == "" {
		return again, nil // Only Karmada may decide the failover target.
	}
	if e := validateGroupRelease(ctx, r.Reader, request); e != nil {
		return again, nil
	}
	if input.Capacity.Mode == "Existing" {
		if r.ExistingNodes == nil {
			return again, fmt.Errorf("existing-capacity member reader is not configured")
		}
		nodes, e := r.ExistingNodes(ctx, input)
		if e != nil {
			return again, e
		}
		pods, _, e := unstructured.NestedSlice(request.Object, "spec", "pods")
		if e != nil || len(nodes) != len(pods) {
			return again, fmt.Errorf("existing target mapping changed")
		}
		for _, raw := range pods {
			pod, ok := raw.(map[string]interface{})
			if !ok || nodes[stringField(pod, "targetPod")] != stringField(pod, "targetNode") {
				return again, fmt.Errorf("existing target mapping differs from restore request")
			}
		}
	}
	before := binding.DeepCopy()
	delete(a, pendingPlacementAnnotation)
	delete(a, applicationFailoverTrigger)
	delete(a, applicationFailoverSourceAnnotation)
	if a[groupVolumeHold] == string(request.GetUID()) {
		unstructured.RemoveNestedField(binding.Object, "spec", "suspension", "dispatching")
		delete(a, groupVolumeHold)
	}
	binding.SetAnnotations(a)
	_ = unstructured.SetNestedSlice(binding.Object, clusters, "spec", "clusters")
	if reflect.DeepEqual(before.Object, binding.Object) {
		return again, nil
	}
	return again, r.Patch(ctx, binding, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
