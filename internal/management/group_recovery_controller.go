package management

import (
	"context"
	"fmt"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type GroupRecoveryReconciler struct {
	client.Client
	Reader client.Reader
}

func (r *GroupRecoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("group-recovery-management").For(newRestoreRequest()).Complete(r)
}
func (r *GroupRecoveryReconciler) Reconcile(ctx context.Context, key ctrl.Request) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: 5 * time.Second}
	req := newRestoreRequest()
	if err := r.Reader.Get(ctx, key.NamespacedName, req); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if req.GetLabels()[p.LabelRole] != groupRestoreRole || !req.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	policy := p.NewObject("TrainingPolicy")
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: req.GetNamespace(), Name: req.GetLabels()[p.LabelPolicy]}, policy); err != nil {
		return again, err
	}
	if string(policy.GetUID()) != req.GetLabels()[p.LabelPolicyUID] || stringField(policy.Object, "spec", "workloadRef", "uid") != stringField(req.Object, "spec", "groupRestore", "sourceWorldUID") {
		return again, fmt.Errorf("group recovery policy/world identity changed")
	}
	if stringField(req.Object, "status", "phase") != "Verified" {
		if err := requestGroupInfrastructureFence(ctx, r.Client, req); err != nil {
			return again, err
		}
		return again, nil
	}
	if err := validateGroupVerified(req); err != nil {
		return again, err
	}
	if policy.GetAnnotations()[groupIntentAnnotation] == stringField(req.Object, "spec", "groupRestore", "operationUID") {
		before := policy.DeepCopy()
		annotations := policy.GetAnnotations()
		delete(annotations, groupIntentAnnotation)
		policy.SetAnnotations(annotations)
		if err := r.Patch(ctx, policy, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return again, err
		}
	}
	a := req.GetAnnotations()
	if a[groupOldName] == "" {
		return ctrl.Result{}, nil
	}
	replacement := p.NewObject("NodeProvision")
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: req.GetNamespace(), Name: a[groupNewName]}, replacement); err != nil {
		return again, err
	}
	if string(replacement.GetUID()) != a[groupNewUID] || replacement.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) || stringField(replacement.Object, "status", "phase") != "Ready" {
		return again, fmt.Errorf("verified replacement identity/readiness mismatch")
	}
	old := p.NewObject("NodeProvision")
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: req.GetNamespace(), Name: a[groupOldName]}, old); apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} else if err != nil {
		return again, err
	}
	if string(old.GetUID()) != a[groupOldUID] || old.GetLabels()[p.LabelPolicyUID] != string(policy.GetUID()) {
		return again, fmt.Errorf("refuse deleting recreated source NodeProvision")
	}
	uid := types.UID(a[groupOldUID])
	if err := r.Delete(ctx, old, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return again, err
	}
	return again, nil
}

func validateGroupVerified(req *unstructured.Unstructured) error {
	if req.GetUID() == "" || req.GetGeneration() < 1 || intField(req.Object, "status", "observedGeneration") != req.GetGeneration() || stringField(req.Object, "status", "phase") != "Verified" {
		return fmt.Errorf("group verification is stale")
	}
	v, _, _ := unstructured.NestedMap(req.Object, "status", "verification")
	for key, want := range map[string]string{
		"requestUID": string(req.GetUID()), "operation": stringField(req.Object, "spec", "groupRestore", "operationUID"),
		"checkpointID":  stringField(req.Object, "spec", "checkpointRef", "checkpointID"),
		"sourceCluster": stringField(req.Object, "spec", "sourceCluster"), "targetCluster": stringField(req.Object, "spec", "targetCluster"),
	} {
		if want == "" || stringField(v, key) != want {
			return fmt.Errorf("group verification %s mismatch", key)
		}
	}
	for _, key := range []string{"name", "uid"} {
		if stringField(v, "trainingRuntimeRef", key) == "" || stringField(v, "trainingRuntimeRef", key) != stringField(req.Object, "spec", "trainingRuntimeRef", key) {
			return fmt.Errorf("verified runtime identity mismatch")
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, stringField(v, "verifiedAt")); err != nil {
		return fmt.Errorf("verification time missing")
	}
	if !boolField(v, "sourceFenced") || !boolField(v, "sourceFence", "fenced") || stringField(v, "sourceFence", "operation") != stringField(v, "operation") || stringField(v, "sourceFence", "evidenceID") == "" {
		return fmt.Errorf("group source fence verification missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, stringField(v, "sourceFence", "observedAt")); err != nil {
		return fmt.Errorf("source fence observation missing")
	}
	return nil
}
