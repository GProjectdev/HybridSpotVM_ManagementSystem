package management

import (
	"context"
	"fmt"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Binding is one-time: deletion/recreation of the workload requires a new request.
type NodeRequestBindingReconciler struct {
	client.Client
	Reader client.Reader
}

func (r *NodeRequestBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("node-request-workload-binding").
		For(p.NewObject("NodeRequest")).Complete(r)
}

func (r *NodeRequestBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: 15 * time.Second}
	request := p.NewObject("NodeRequest")
	if err := r.Reader.Get(ctx, req.NamespacedName, request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !request.GetDeletionTimestamp().IsZero() || stringField(request.Object, "spec", "workloadRef", "uid") != "" {
		return ctrl.Result{}, nil
	}
	input := p.ReadPolicyInput(request)
	if input.WorkloadRef.APIVersion != "apps/v1" || input.WorkloadRef.Kind != "StatefulSet" || input.WorkloadRef.Name == "" {
		return ctrl.Result{}, fmt.Errorf("NodeRequest currently supports a named apps/v1 StatefulSet")
	}
	workload := p.NewObject("StatefulSet")
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: request.GetNamespace(), Name: input.WorkloadRef.Name}, workload); apierrors.IsNotFound(err) {
		return again, nil
	} else if err != nil {
		return again, err
	}
	if workload.GetUID() == "" || !workload.GetDeletionTimestamp().IsZero() {
		return again, nil
	}
	if err := validateUniqueWorkloadRequest(ctx, r.Reader, request); err != nil {
		return again, err
	}
	before := request.DeepCopy()
	if err := unstructured.SetNestedField(request.Object, string(workload.GetUID()), "spec", "workloadRef", "uid"); err != nil {
		return again, err
	}
	return again, r.Patch(ctx, request, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// Initial allocation is allowed before the workload exists, but replacement
// and checkpoint/restore still require the normal UID and placement gates.
func (r *PolicyReconciler) provisionBeforeWorkload(ctx context.Context, request *unstructured.Unstructured, input p.PolicyInput, decision p.Decision) (ctrl.Result, error) {
	if err := validateUniqueWorkloadRequest(ctx, r.reader(), request); err != nil {
		return ctrl.Result{}, err
	}
	if input.WorkloadRef.APIVersion != "apps/v1" || input.WorkloadRef.Kind != "StatefulSet" || input.WorkloadRef.Name == "" || input.SourceCluster != input.Capacity.AWSCluster {
		return ctrl.Result{}, fmt.Errorf("unbound NodeRequest requires a named StatefulSet targeting its public cloud member")
	}
	initial := request.DeepCopy()
	_ = unstructured.SetNestedField(initial.Object, false, "spec", "replacement", "enabled")
	capacity, err := r.checkCapacityLifecycle(ctx, initial, input, decision)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !capacity.Blocked && !decision.ProvisioningBlocked {
		if err := r.ensureNodeProvisions(ctx, input, decision); err != nil {
			return ctrl.Result{}, err
		}
	}
	status := p.PolicyStatus(decision, r.now())
	status["provisioningBlocked"] = capacity.Blocked || decision.ProvisioningBlocked
	status["awaitingWorkload"] = true
	if capacity.Blocked {
		status["reason"] = capacity.Reason
	}
	applyCapacityStatus(status, capacity)
	return ctrl.Result{RequeueAfter: 15 * time.Second}, patchStatusSubtree(ctx, r.Client, request, p.StatusPolicyPath, status)
}

func validateUniqueWorkloadRequest(ctx context.Context, reader client.Reader, request *unstructured.Unstructured) error {
	requests := p.NewList("NodeRequest")
	if err := reader.List(ctx, requests, client.InNamespace(request.GetNamespace())); err != nil {
		return err
	}
	input := p.ReadPolicyInput(request)
	for _, other := range requests.Items {
		ref := p.ReadPolicyInput(&other).WorkloadRef
		if other.GetName() != request.GetName() && ref.APIVersion == input.WorkloadRef.APIVersion && ref.Kind == input.WorkloadRef.Kind && ref.Name == input.WorkloadRef.Name {
			return fmt.Errorf("multiple NodeRequests reference workload; allocation and binding refused")
		}
	}
	return nil
}
