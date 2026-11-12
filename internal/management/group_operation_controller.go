package management

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const groupStageCapacity = "capacity"
const groupStageRequest = "request"
const groupStageIntent = "intent"
const groupOperationAnnotation = "training.dcnlab.com/group-operation"

// An immutable intent for one policy generation. It is retained after recovery
// for restart deduplication and replaced only after verified prior recovery.
type groupOperation struct {
	PolicyUID        string
	PolicyGeneration int64
	OldName          string
	OldUID           string
	Operation        string
	Replacement      string
	Market           string
	Event            string
}

func (r *PolicyReconciler) recordGroupOperation(ctx context.Context, policy, old *unstructured.Unstructured, operation, replacement, market, event string) error {
	intent := groupOperation{string(policy.GetUID()), policy.GetGeneration(), old.GetName(), string(old.GetUID()), operation, replacement, market, event}
	if intent.PolicyUID == "" || intent.OldUID == "" || intent.Operation == "" || intent.Replacement == "" || !validReplacementMarket(market) {
		return fmt.Errorf("incomplete group operation identity")
	}
	current := p.NewObject("TrainingPolicy")
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
		return err
	}
	if current.GetUID() != policy.GetUID() || current.GetGeneration() != policy.GetGeneration() || !current.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("group policy identity changed")
	}
	annotations := current.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if raw := annotations[groupOperationAnnotation]; raw != "" {
		var previous groupOperation
		if err := json.Unmarshal([]byte(raw), &previous); err != nil {
			return err
		}
		if previous == intent {
			return nil
		}
		prior := newRestoreRequest()
		if err := r.reader().Get(ctx, client.ObjectKey{Namespace: policy.GetNamespace(), Name: previous.Operation + "-group-restore"}, prior); err != nil {
			return err
		}
		if prior.GetLabels()[p.LabelPolicyUID] != previous.PolicyUID || prior.GetAnnotations()[groupOldUID] != previous.OldUID {
			return fmt.Errorf("prior group operation identity mismatch")
		}
		if err := validateGroupVerified(prior); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	before := current.DeepCopy()
	annotations[groupOperationAnnotation] = string(raw)
	current.SetAnnotations(annotations)
	return r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

type GroupCapacityReconciler struct{ *PolicyReconciler }
type GroupRequestReconciler struct{ *PolicyReconciler }

func (r *GroupCapacityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("group-replacement-capacity").For(p.NewObject("TrainingPolicy")).Complete(r)
}
func (r *GroupRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("group-replacement-request").For(p.NewObject("TrainingPolicy")).Complete(r)
}
func (r *GroupCapacityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcileGroupOperation(ctx, req, groupStageCapacity)
}
func (r *GroupRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcileGroupOperation(ctx, req, groupStageRequest)
}

func (r *PolicyReconciler) reconcileGroupOperation(ctx context.Context, req ctrl.Request, stage string) (ctrl.Result, error) {
	policy := p.NewObject("TrainingPolicy")
	if err := r.reader().Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !policy.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	raw := policy.GetAnnotations()[groupOperationAnnotation]
	if raw == "" {
		return ctrl.Result{}, nil
	}
	again := ctrl.Result{RequeueAfter: 5 * time.Second}
	var intent groupOperation
	if err := json.Unmarshal([]byte(raw), &intent); err != nil {
		return again, err
	}
	if intent.PolicyUID != string(policy.GetUID()) || intent.PolicyGeneration != policy.GetGeneration() {
		return again, fmt.Errorf("stale group operation policy identity")
	}
	request := newRestoreRequest()
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: policy.GetNamespace(), Name: intent.Operation + "-group-restore"}, request); err == nil {
		if request.GetLabels()[p.LabelPolicyUID] != intent.PolicyUID || request.GetAnnotations()[groupOldUID] != intent.OldUID {
			return again, fmt.Errorf("group request identity mismatch")
		}
		return ctrl.Result{}, nil
	} else if !apierrors.IsNotFound(err) {
		return again, err
	}
	input := p.ReadPolicyInput(policy)
	if input.Capacity.Mode == p.CapacityModeExisting || input.Suspended || !boolField(policy.Object, "spec", "replacement", "enabled") {
		return again, fmt.Errorf("group replacement capacity is disabled")
	}
	if err := validatePolicyInput(input); err != nil {
		return again, err
	}
	if err := r.validateLiveWorkloadUID(ctx, input); err != nil {
		return again, err
	}
	old := p.NewObject("NodeProvision")
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: policy.GetNamespace(), Name: intent.OldName}, old); err != nil {
		return again, err
	}
	if string(old.GetUID()) != intent.OldUID || old.GetLabels()[p.LabelPolicyUID] != intent.PolicyUID || !old.GetDeletionTimestamp().IsZero() ||
		intent.Operation != replacementOperationName(old.GetName(), intent.OldUID) || intent.Replacement != replacementNodeProvisionName(old.GetName(), intent.OldUID) || !validReplacementMarket(intent.Market) {
		return again, fmt.Errorf("group capacity intent identity mismatch")
	}
	if intent.Event != "" {
		event, valid := readEmergencySpotEvent(old, input)
		if !valid || event.EventID != intent.Event || !validSpotSignalType(stringField(old.Object, "status", "spot", "signalType")) {
			return again, fmt.Errorf("group emergency event changed")
		}
	}
	producer := *r
	producer.GroupStage = stage
	_, err := producer.ensureGroupReplacement(ctx, policy, input, old, intent.Operation, intent.Replacement, intent.Market, intent.Event)
	return again, err
}
