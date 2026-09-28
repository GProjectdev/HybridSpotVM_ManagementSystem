package management

import (
	"context"
	"fmt"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	replacementPhaseAwaitingReplacementReady  = "AwaitingReplacementReady"
	replacementPhaseAwaitingPartialCheckpoint = "AwaitingPartialCheckpoint"
	replacementPhaseAwaitingRestoreEvidence   = "AwaitingRestoreEvidence"
)

type ReplacementReconciler struct {
	client.Client
	APIReader client.Reader
	Clock     func() time.Time
}

type replacementSpec struct {
	Operation                    string
	PolicyName                   string
	PolicyUID                    string
	PolicyGeneration             int64
	WorkloadAPIVersion           string
	WorkloadKind                 string
	WorkloadName                 string
	WorkloadUID                  string
	SourceCluster                string
	TargetCluster                string
	OldNodeProvisionName         string
	OldNodeProvisionUID          string
	ReplacementNodeProvisionName string
	DesiredMarketType            string
	EmergencyEventID             string
	TargetRanks                  []int64
	Pods                         []interface{}
	PreservedSurvivors           []interface{}
}

func (r *ReplacementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("spot-replacement-management").
		For(newSpotReplacementObject()).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}

func (r *ReplacementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	op := newSpotReplacementObject()
	if err := r.Get(ctx, req.NamespacedName, op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !op.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	phase, err := r.reconcileReplacement(ctx, op)
	status := map[string]interface{}{
		"observedGeneration": op.GetGeneration(),
		"phase":              phase,
		"observedAt":         r.now().UTC().Format(time.RFC3339),
	}
	if err != nil {
		status["message"] = err.Error()
	}
	if refErr := r.applyReplacementStatusRefs(ctx, op.GetNamespace(), readReplacementRefs(op), status); refErr != nil && err == nil {
		status["message"] = refErr.Error()
	}
	if patchErr := patchReplacementStatus(ctx, r.Client, op, status); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	if err != nil && phase != "Rejected" {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if phase == replacementPhaseAwaitingReplacementReady || phase == replacementPhaseAwaitingPartialCheckpoint || phase == replacementPhaseAwaitingRestoreEvidence {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

type replacementRefs struct {
	ReplacementNodeProvisionName string
	PartialCheckpointName        string
	RestoreRequestName           string
	RecoveryName                 string
}

func readReplacementRefs(op *unstructured.Unstructured) replacementRefs {
	return replacementRefs{
		ReplacementNodeProvisionName: stringField(op.Object, "spec", "replacementNodeProvisionRef", "name"),
		PartialCheckpointName:        stringField(op.Object, "spec", "operation") + "-partial-checkpoint",
		RestoreRequestName:           stringField(op.Object, "spec", "operation") + "-restore",
		RecoveryName:                 stringField(op.Object, "spec", "operation") + "-cleanup",
	}
}

func (r *ReplacementReconciler) applyReplacementStatusRefs(ctx context.Context, ns string, refs replacementRefs, status map[string]interface{}) error {
	if refs.ReplacementNodeProvisionName != "" {
		np := trainingpolicy.NewObject("NodeProvision")
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: refs.ReplacementNodeProvisionName}, np)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get replacement NodeProvision status ref: %w", err)
		}
		if err == nil {
			status["replacementNodeProvisionRef"] = map[string]interface{}{"name": np.GetName(), "uid": string(np.GetUID())}
		}
	}
	if refs.PartialCheckpointName != "-partial-checkpoint" {
		migration := trainingpolicy.NewObject("FluidCRMigration")
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: refs.PartialCheckpointName}, migration)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get partial checkpoint status ref: %w", err)
		}
		if err == nil {
			status["partialCheckpointRef"] = map[string]interface{}{
				"name":         migration.GetName(),
				"checkpointID": migration.GetAnnotations()["training.dcnlab.com/checkpoint-id"],
			}
		}
	}
	if refs.RestoreRequestName != "-restore" {
		req := newRestoreRequest()
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: refs.RestoreRequestName}, req)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get RestoreRequest status ref: %w", err)
		}
		if err == nil {
			status["restoreRequestRef"] = map[string]interface{}{"name": req.GetName(), "uid": string(req.GetUID()), "generation": req.GetGeneration()}
		}
	}
	if refs.RecoveryName != "-cleanup" {
		recovery := trainingpolicy.NewObject("SpotRecovery")
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: refs.RecoveryName}, recovery)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get SpotRecovery status ref: %w", err)
		}
		if err == nil {
			status["spotRecoveryRef"] = map[string]interface{}{"name": recovery.GetName(), "uid": string(recovery.GetUID()), "phase": stringField(recovery.Object, "status", "phase")}
		}
	}
	return nil
}

func (r *ReplacementReconciler) reconcileReplacement(ctx context.Context, op *unstructured.Unstructured) (string, error) {
	spec, err := readReplacementSpec(op)
	if err != nil {
		return "Rejected", err
	}
	if err := r.verifyReplacementPolicy(ctx, op.GetNamespace(), spec); err != nil {
		return "Rejected", err
	}
	if phase, found, err := r.existingSpotRecoveryPhase(ctx, op.GetNamespace(), spec); found || err != nil {
		if err != nil {
			return replacementPhaseAwaitingRestoreEvidence, err
		}
		switch phase {
		case "Completed":
			return "Completed", nil
		case "Rejected":
			return "Rejected", fmt.Errorf("SpotRecovery cleanup rejected")
		default:
			return replacementPhaseAwaitingRestoreEvidence, fmt.Errorf("waiting for SpotRecovery cleanup completion")
		}
	}
	oldNP := trainingpolicy.NewObject("NodeProvision")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: op.GetNamespace(), Name: spec.OldNodeProvisionName}, oldNP); err != nil {
		return "Rejected", fmt.Errorf("get old NodeProvision: %w", err)
	}
	if err := verifyReplacementOldNode(oldNP, spec); err != nil {
		return "Rejected", err
	}
	replacement, created, err := r.ensureReplacementNodeProvision(ctx, op.GetNamespace(), oldNP, spec)
	if err != nil {
		return "Pending", err
	}
	var migration *unstructured.Unstructured
	if spec.EmergencyEventID != "" {
		migration, _, err = r.ensureReplacementPartialCheckpoint(ctx, op.GetNamespace(), spec)
		if err != nil {
			return replacementPhaseAwaitingPartialCheckpoint, err
		}
	}
	if created {
		return replacementPhaseAwaitingReplacementReady, nil
	}
	if err := verifyReplacementReadyForOperation(replacement, spec); err != nil {
		return replacementPhaseAwaitingReplacementReady, err
	}
	if migration == nil {
		var checkpointCreated bool
		migration, checkpointCreated, err = r.ensureReplacementPartialCheckpoint(ctx, op.GetNamespace(), spec)
		if err != nil {
			return replacementPhaseAwaitingPartialCheckpoint, err
		}
		if checkpointCreated {
			return replacementPhaseAwaitingPartialCheckpoint, nil
		}
	}
	if err := verifyPartialCheckpointEvidence(migration, spec); err != nil {
		return replacementPhaseAwaitingPartialCheckpoint, err
	}
	_, runtimeObj, err := r.replacementPolicyAndRuntime(ctx, op.GetNamespace(), spec)
	if err != nil {
		return replacementPhaseAwaitingRestoreEvidence, err
	}
	restore, created, err := r.ensureRestoreRequest(ctx, op.GetNamespace(), spec, migration, replacement)
	if err != nil {
		return replacementPhaseAwaitingRestoreEvidence, err
	}
	if created {
		return replacementPhaseAwaitingRestoreEvidence, fmt.Errorf("waiting for Stateful RestoreRequest/RestorePlan staged-ready and member actuator fencing evidence")
	}
	recoverySpec := replacementRecoverySpec(spec, runtimeObj, restore, migration, oldNP, replacement)
	recoveryVerifier := &RecoveryReconciler{Client: r.Client, APIReader: r.reader(), Clock: r.Clock}
	if _, err := recoveryVerifier.verifyRestoreRequest(ctx, op.GetNamespace(), recoverySpec); err != nil {
		return replacementPhaseAwaitingRestoreEvidence, err
	}
	recovery, created, err := r.ensureSpotRecovery(ctx, op.GetNamespace(), spec, recoverySpec)
	if err != nil {
		return replacementPhaseAwaitingRestoreEvidence, err
	}
	if created {
		return replacementPhaseAwaitingRestoreEvidence, fmt.Errorf("waiting for SpotRecovery cleanup gate")
	}
	switch phase := stringField(recovery.Object, "status", "phase"); phase {
	case "Completed":
		return "Completed", nil
	case "Rejected":
		return "Rejected", fmt.Errorf("SpotRecovery cleanup rejected: %s", stringField(recovery.Object, "status", "message"))
	default:
		return replacementPhaseAwaitingRestoreEvidence, fmt.Errorf("waiting for SpotRecovery cleanup completion")
	}
}

func (r *ReplacementReconciler) existingSpotRecoveryPhase(ctx context.Context, ns string, spec replacementSpec) (string, bool, error) {
	recovery := trainingpolicy.NewObject("SpotRecovery")
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.Operation + "-cleanup"}, recovery)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get SpotRecovery cleanup receipt: %w", err)
	}
	if recovery.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID {
		return "", true, fmt.Errorf("existing SpotRecovery cleanup ownership mismatch")
	}
	if stringField(recovery.Object, "spec", "operation") != spec.Operation ||
		stringField(recovery.Object, "spec", "oldNodeProvisionRef", "name") != spec.OldNodeProvisionName ||
		stringField(recovery.Object, "spec", "oldNodeProvisionRef", "uid") != spec.OldNodeProvisionUID ||
		stringField(recovery.Object, "spec", "replacementNodeProvisionRef", "name") != spec.ReplacementNodeProvisionName {
		return "", true, fmt.Errorf("existing SpotRecovery cleanup receipt does not match replacement operation")
	}
	return stringField(recovery.Object, "status", "phase"), true, nil
}

func readReplacementSpec(op *unstructured.Unstructured) (replacementSpec, error) {
	spec := replacementSpec{
		Operation:                    stringField(op.Object, "spec", "operation"),
		PolicyName:                   stringField(op.Object, "spec", "policyRef", "name"),
		PolicyUID:                    stringField(op.Object, "spec", "policyRef", "uid"),
		PolicyGeneration:             intField(op.Object, "spec", "policyRef", "generation"),
		WorkloadAPIVersion:           stringField(op.Object, "spec", "workloadRef", "apiVersion"),
		WorkloadKind:                 stringField(op.Object, "spec", "workloadRef", "kind"),
		WorkloadName:                 stringField(op.Object, "spec", "workloadRef", "name"),
		WorkloadUID:                  stringField(op.Object, "spec", "workloadRef", "uid"),
		SourceCluster:                stringField(op.Object, "spec", "sourceCluster"),
		TargetCluster:                stringField(op.Object, "spec", "targetCluster"),
		OldNodeProvisionName:         stringField(op.Object, "spec", "oldNodeProvisionRef", "name"),
		OldNodeProvisionUID:          stringField(op.Object, "spec", "oldNodeProvisionRef", "uid"),
		ReplacementNodeProvisionName: stringField(op.Object, "spec", "replacementNodeProvisionRef", "name"),
		DesiredMarketType:            stringField(op.Object, "spec", "desiredMarketType"),
		EmergencyEventID:             op.GetAnnotations()["training.dcnlab.com/emergency-event-id"],
	}
	rawRanks, ok, _ := unstructured.NestedSlice(op.Object, "spec", "partialCheckpoint", "targetRanks")
	if ok {
		for _, raw := range rawRanks {
			if rank, ok := int64Value(raw); ok {
				spec.TargetRanks = append(spec.TargetRanks, rank)
			}
		}
	}
	spec.Pods, _, _ = unstructured.NestedSlice(op.Object, "spec", "pods")
	spec.PreservedSurvivors, _, _ = unstructured.NestedSlice(op.Object, "spec", "partialRestore", "preservedSurvivors")
	if spec.Operation == "" || spec.PolicyName == "" || spec.PolicyUID == "" || spec.PolicyGeneration <= 0 {
		return spec, fmt.Errorf("operation and policyRef name/uid/generation are required")
	}
	if spec.WorkloadAPIVersion != "apps/v1" || spec.WorkloadKind != "StatefulSet" || spec.WorkloadName == "" || spec.WorkloadUID == "" {
		return spec, fmt.Errorf("StatefulSet workloadRef apiVersion/kind/name/uid is required")
	}
	if spec.SourceCluster != awsNodeProvisionCluster || spec.TargetCluster != awsNodeProvisionCluster {
		return spec, fmt.Errorf("SpotReplacement currently supports only same-cluster aws replacement")
	}
	if spec.OldNodeProvisionName == "" || spec.OldNodeProvisionUID == "" || spec.ReplacementNodeProvisionName == "" {
		return spec, fmt.Errorf("old and replacement NodeProvision refs are required")
	}
	if spec.OldNodeProvisionName == spec.ReplacementNodeProvisionName {
		return spec, fmt.Errorf("old and replacement NodeProvision names must differ")
	}
	if !validReplacementMarket(spec.DesiredMarketType) {
		return spec, fmt.Errorf("desiredMarketType must be Spot or OnDemand")
	}
	if !boolField(op.Object, "spec", "partialRestore", "preventPeriodicResume") {
		return spec, fmt.Errorf("partialRestore.preventPeriodicResume=true is required")
	}
	if len(spec.TargetRanks) == 0 {
		return spec, fmt.Errorf("partialCheckpoint.targetRanks is required")
	}
	if err := verifyReplacementPodEvidence(spec); err != nil {
		return spec, err
	}
	return spec, nil
}

func verifyReplacementPodEvidence(spec replacementSpec) error {
	ranks := map[int64]bool{}
	for _, rank := range spec.TargetRanks {
		if rank == 0 {
			return fmt.Errorf("UnsupportedRankZero: partial replacement of rank 0 is not supported")
		}
		if rank < 0 {
			return fmt.Errorf("partialCheckpoint target rank must be non-negative")
		}
		ranks[rank] = true
	}
	seen := map[int64]bool{}
	for _, item := range spec.Pods {
		pod, ok := item.(map[string]interface{})
		rank := intField(pod, "rank")
		if !ok || !ranks[rank] || stringField(pod, "sourcePodUID") == "" || firstString(pod, [][]string{{"sourcePod"}, {"sourcePodName"}}) == "" || stringField(pod, "sourceNode") == "" {
			return fmt.Errorf("spec.pods rank/sourcePod/sourcePodUID/sourceNode evidence is required for every target rank")
		}
		seen[rank] = true
	}
	for rank := range ranks {
		if !seen[rank] {
			return fmt.Errorf("spec.pods missing target rank evidence")
		}
	}
	for _, item := range spec.PreservedSurvivors {
		survivor, ok := item.(map[string]interface{})
		if !ok || intField(survivor, "rank") < 0 || stringField(survivor, "podName") == "" || stringField(survivor, "podUID") == "" || stringField(survivor, "nodeName") == "" {
			return fmt.Errorf("partialRestore.preservedSurvivors require baseline rank,podName,podUID,nodeName")
		}
	}
	return nil
}

func (r *ReplacementReconciler) verifyReplacementPolicy(ctx context.Context, ns string, spec replacementSpec) error {
	policy := trainingpolicy.NewObject("TrainingPolicy")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.PolicyName}, policy); err != nil {
		return fmt.Errorf("get TrainingPolicy: %w", err)
	}
	if string(policy.GetUID()) != spec.PolicyUID || policy.GetGeneration() != spec.PolicyGeneration {
		return fmt.Errorf("policyRef does not match current TrainingPolicy uid/generation")
	}
	if stringField(policy.Object, "spec", "workloadRef", "uid") != spec.WorkloadUID {
		return fmt.Errorf("TrainingPolicy workloadRef.uid mismatch")
	}
	return nil
}

func verifyReplacementOldNode(oldNP *unstructured.Unstructured, spec replacementSpec) error {
	if _, requested, _ := unstructured.NestedMap(oldNP.Object, "spec", "fence"); requested {
		return fmt.Errorf("partial replacement cannot use an infrastructure-fenced source")
	}
	if _, recorded, _ := unstructured.NestedMap(oldNP.Object, "status", "fence"); recorded {
		return fmt.Errorf("partial replacement cannot use a source with infrastructure fence evidence")
	}
	if !oldNP.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("old NodeProvision is deleting")
	}
	if string(oldNP.GetUID()) != spec.OldNodeProvisionUID {
		return fmt.Errorf("old NodeProvision uid mismatch")
	}
	if oldNP.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID {
		return fmt.Errorf("old NodeProvision policy uid mismatch")
	}
	if market := stringField(oldNP.Object, "spec", "marketType"); !validReplacementMarket(market) || market == spec.DesiredMarketType {
		return fmt.Errorf("old NodeProvision marketType must be Spot/OnDemand and differ from desiredMarketType")
	}
	if spec.EmergencyEventID != "" {
		if spec.DesiredMarketType != "OnDemand" || stringField(oldNP.Object, "spec", "marketType") != "Spot" {
			return fmt.Errorf("emergency replacement requires Spot to OnDemand replacement")
		}
		if spec.Operation != replacementOperationName(spec.OldNodeProvisionName, spec.OldNodeProvisionUID) {
			return fmt.Errorf("emergency replacement operation must be UID-bound to old NodeProvision")
		}
		instanceID := stringField(oldNP.Object, "status", "instanceId")
		spot, ok, _ := unstructured.NestedMap(oldNP.Object, "status", "spot")
		atRisk, _, _ := unstructured.NestedBool(map[string]interface{}{"spot": spot}, "spot", "atRisk")
		if !ok || instanceID == "" || stringField(spot, "eventID") != spec.EmergencyEventID || stringField(spot, "instanceID") != instanceID || !atRisk || !validSpotSignalType(stringField(spot, "signalType")) {
			return fmt.Errorf("emergency replacement requires old NodeProvision status.spot event evidence matching annotation")
		}
	}
	return nil
}

func (r *ReplacementReconciler) ensureReplacementNodeProvision(ctx context.Context, ns string, oldNP *unstructured.Unstructured, spec replacementSpec) (*unstructured.Unstructured, bool, error) {
	existing := trainingpolicy.NewObject("NodeProvision")
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.ReplacementNodeProvisionName}, existing)
	if err == nil {
		if existing.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID {
			return nil, false, fmt.Errorf("existing replacement NodeProvision policy uid mismatch")
		}
		input := trainingpolicy.PolicyInput{Namespace: ns, PolicyName: spec.PolicyName, PolicyUID: types.UID(spec.PolicyUID)}
		if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, existing, spec.TargetCluster)); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	desired := trainingpolicy.NewObject("NodeProvision")
	desired.SetNamespace(ns)
	desired.SetName(spec.ReplacementNodeProvisionName)
	desired.SetLabels(map[string]string{
		trainingpolicy.LabelManagedBy: "hybridspotvm-system",
		trainingpolicy.LabelPolicy:    spec.PolicyName,
		trainingpolicy.LabelPolicyUID: spec.PolicyUID,
		trainingpolicy.LabelRole:      "replacement",
	})
	desired.SetAnnotations(map[string]string{
		"training.dcnlab.com/recovery-operation":         spec.Operation,
		"training.dcnlab.com/replaces-nodeprovision":     spec.OldNodeProvisionName,
		"training.dcnlab.com/replaces-nodeprovision-uid": spec.OldNodeProvisionUID,
	})
	sourceSpec, ok, _ := unstructured.NestedMap(oldNP.Object, "spec")
	if !ok {
		return nil, false, fmt.Errorf("old NodeProvision spec required")
	}
	newSpec := deepCopyMap(sourceSpec)
	newSpec["marketType"] = spec.DesiredMarketType
	newSpec["hostname"] = spec.ReplacementNodeProvisionName
	desired.Object["spec"] = newSpec
	if err := r.Create(ctx, desired); err != nil {
		return nil, false, err
	}
	input := trainingpolicy.PolicyInput{Namespace: ns, PolicyName: spec.PolicyName, PolicyUID: types.UID(spec.PolicyUID)}
	if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, desired, spec.TargetCluster)); err != nil {
		return nil, false, err
	}
	return desired, true, nil
}

func verifyReplacementReadyForOperation(replacement *unstructured.Unstructured, spec replacementSpec) error {
	if stringField(replacement.Object, "spec", "marketType") != spec.DesiredMarketType {
		return fmt.Errorf("replacement NodeProvision marketType must be %s", spec.DesiredMarketType)
	}
	if string(replacement.GetUID()) == spec.OldNodeProvisionUID {
		return fmt.Errorf("replacement NodeProvision uid must differ from old uid")
	}
	if phase := stringField(replacement.Object, "status", "phase"); phase != "Ready" {
		return fmt.Errorf("replacement NodeProvision is not Ready")
	}
	if stringField(replacement.Object, "status", "instanceId") == "" {
		return fmt.Errorf("replacement NodeProvision instanceId evidence required")
	}
	if op := replacement.GetAnnotations()["training.dcnlab.com/recovery-operation"]; op != spec.Operation {
		return fmt.Errorf("replacement NodeProvision operation mismatch")
	}
	return nil
}

func (r *ReplacementReconciler) ensureReplacementPartialCheckpoint(ctx context.Context, ns string, spec replacementSpec) (*unstructured.Unstructured, bool, error) {
	if err := r.waitForExistingCheckpointsTerminal(ctx, ns, spec); err != nil {
		return nil, false, err
	}
	return r.ensurePartialCheckpoint(ctx, ns, spec)
}

func (r *ReplacementReconciler) ensurePartialCheckpoint(ctx context.Context, ns string, spec replacementSpec) (*unstructured.Unstructured, bool, error) {
	name := spec.Operation + "-partial-checkpoint"
	existing := trainingpolicy.NewObject("FluidCRMigration")
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, existing)
	if err == nil {
		if existing.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID || existing.GetLabels()[trainingpolicy.LabelRole] != "replacement-checkpoint" {
			return nil, false, fmt.Errorf("existing partial checkpoint ownership mismatch")
		}
		input := trainingpolicy.PolicyInput{Namespace: ns, PolicyName: spec.PolicyName, PolicyUID: types.UID(spec.PolicyUID)}
		if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, existing, spec.SourceCluster)); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	desired := trainingpolicy.NewObject("FluidCRMigration")
	desired.SetNamespace(ns)
	desired.SetName(name)
	desired.SetLabels(map[string]string{
		trainingpolicy.LabelManagedBy: "hybridspotvm-system",
		trainingpolicy.LabelPolicy:    spec.PolicyName,
		trainingpolicy.LabelPolicyUID: spec.PolicyUID,
		trainingpolicy.LabelRole:      "replacement-checkpoint",
	})
	desired.SetAnnotations(map[string]string{
		"training.dcnlab.com/recovery-operation": spec.Operation,
		"training.dcnlab.com/started-at":         r.now().UTC().Format(time.RFC3339),
		"training.dcnlab.com/checkpoint-id":      name,
	})
	desired.Object["spec"] = map[string]interface{}{
		"workloadRef": map[string]interface{}{
			"apiVersion": spec.WorkloadAPIVersion,
			"kind":       spec.WorkloadKind,
			"name":       spec.WorkloadName,
			"uid":        spec.WorkloadUID,
		},
		"resume": false,
		"partialCheckpoint": map[string]interface{}{
			"targetRanks": int64SliceToInterface(spec.TargetRanks),
		},
		"pods": spec.Pods,
	}
	if err := r.Create(ctx, desired); err != nil {
		return nil, false, err
	}
	input := trainingpolicy.PolicyInput{Namespace: ns, PolicyName: spec.PolicyName, PolicyUID: types.UID(spec.PolicyUID)}
	if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, desired, spec.SourceCluster)); err != nil {
		return nil, false, err
	}
	return desired, true, nil
}

func verifyPartialCheckpointEvidence(migration *unstructured.Unstructured, spec replacementSpec) error {
	if resume, _, _ := unstructured.NestedBool(migration.Object, "spec", "resume"); resume {
		return fmt.Errorf("partial checkpoint FluidCRMigration spec.resume must be false")
	}
	if !isTerminalPhase(migration, spec.SourceCluster) {
		return fmt.Errorf("partial checkpoint is not Completed for source cluster")
	}
	_, err := partialCheckpointArchiveEvidence(migration, spec)
	return err
}

func (r *ReplacementReconciler) waitForExistingCheckpointsTerminal(ctx context.Context, ns string, spec replacementSpec) error {
	list := trainingpolicy.NewList("FluidCRMigration")
	labels := client.MatchingLabels{trainingpolicy.LabelPolicyUID: spec.PolicyUID, trainingpolicy.LabelRole: "checkpoint"}
	if err := r.reader().List(ctx, list, client.InNamespace(ns), labels); err != nil {
		return fmt.Errorf("list existing FluidCRMigration checkpoints: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if !isTerminalPhase(item, spec.SourceCluster) {
			return fmt.Errorf("waiting for existing checkpoint %s to reach terminal phase before partial replacement", item.GetName())
		}
	}
	return nil
}

func (r *ReplacementReconciler) replacementPolicyAndRuntime(ctx context.Context, ns string, spec replacementSpec) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	policy := trainingpolicy.NewObject("TrainingPolicy")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.PolicyName}, policy); err != nil {
		return nil, nil, fmt.Errorf("get TrainingPolicy: %w", err)
	}
	input := trainingpolicy.ReadPolicySpec(policy)
	runtimeObj := trainingpolicy.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: input.RuntimeRefName}, runtimeObj); err != nil {
		return nil, nil, fmt.Errorf("get TrainingRuntime: %w", err)
	}
	if stringField(runtimeObj.Object, "spec", "workloadRef", "uid") != spec.WorkloadUID {
		return nil, nil, fmt.Errorf("TrainingRuntime workloadRef.uid mismatch")
	}
	return policy, runtimeObj, nil
}

func (r *ReplacementReconciler) ensureRestoreRequest(ctx context.Context, ns string, spec replacementSpec, migration, replacement *unstructured.Unstructured) (*unstructured.Unstructured, bool, error) {
	name := spec.Operation + "-restore"
	existing := newRestoreRequest()
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, existing)
	if err == nil {
		if existing.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID || existing.GetLabels()[trainingpolicy.LabelRole] != "replacement-restore" {
			return nil, false, fmt.Errorf("existing RestoreRequest ownership mismatch")
		}
		input := trainingpolicy.PolicyInput{Namespace: ns, PolicyName: spec.PolicyName, PolicyUID: types.UID(spec.PolicyUID)}
		if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, existing, spec.TargetCluster)); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	targetNode := firstString(replacement.Object, [][]string{{"status", "nodeName"}, {"spec", "nodeName"}, {"spec", "hostname"}})
	if targetNode == "" {
		return nil, false, fmt.Errorf("replacement NodeProvision target node evidence required")
	}
	pods, err := restoreRequestPods(spec, migration, targetNode)
	if err != nil {
		return nil, false, err
	}
	survivors, err := restoreRequestSurvivors(spec, migration)
	if err != nil {
		return nil, false, err
	}
	desired := newRestoreRequest()
	desired.SetNamespace(ns)
	desired.SetName(name)
	desired.SetLabels(map[string]string{
		trainingpolicy.LabelManagedBy: "hybridspotvm-system",
		trainingpolicy.LabelPolicy:    spec.PolicyName,
		trainingpolicy.LabelPolicyUID: spec.PolicyUID,
		trainingpolicy.LabelRole:      "replacement-restore",
	})
	desired.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": spec.Operation})
	desired.Object["spec"] = map[string]interface{}{
		"sourceCluster": spec.SourceCluster,
		"targetCluster": spec.TargetCluster,
		"sourceFenced":  false,
		"volumesReady":  true,
		"workloadRef": map[string]interface{}{
			"apiVersion": spec.WorkloadAPIVersion,
			"kind":       spec.WorkloadKind,
			"name":       spec.WorkloadName,
			"uid":        spec.WorkloadUID,
		},
		"checkpointRef": map[string]interface{}{
			"name":         migration.GetName(),
			"uid":          string(migration.GetUID()),
			"generation":   migration.GetGeneration(),
			"checkpointID": migration.GetAnnotations()["training.dcnlab.com/checkpoint-id"],
		},
		"partialRestore": map[string]interface{}{
			"preventPeriodicResume": true,
			"targetRanks":           int64SliceToInterface(spec.TargetRanks),
			"preservedSurvivors":    survivors,
		},
		"pods": pods,
	}
	if err := r.Create(ctx, desired); err != nil {
		return nil, false, err
	}
	input := trainingpolicy.PolicyInput{Namespace: ns, PolicyName: spec.PolicyName, PolicyUID: types.UID(spec.PolicyUID)}
	if err := r.createIfMissing(ctx, trainingpolicy.NewPropagationPolicyFor(input, desired, spec.TargetCluster)); err != nil {
		return nil, false, err
	}
	return desired, true, nil
}

func restoreRequestPods(spec replacementSpec, migration *unstructured.Unstructured, targetNode string) ([]interface{}, error) {
	archives, err := partialCheckpointArchiveEvidence(migration, spec)
	if err != nil {
		return nil, err
	}
	out := make([]interface{}, 0, len(spec.TargetRanks))
	for _, item := range spec.Pods {
		pod := item.(map[string]interface{})
		rank := intField(pod, "rank")
		if !requiredRank(spec.TargetRanks, rank) {
			continue
		}
		if len(archives[rank]) == 0 {
			return nil, fmt.Errorf("partial checkpoint archive evidence missing for rank %d", rank)
		}
		sourcePod := firstString(pod, [][]string{{"sourcePod"}, {"sourcePodName"}})
		targetPod := firstString(pod, [][]string{{"targetPod"}, {"targetPodName"}})
		if targetPod == "" {
			targetPod = sourcePod
		}
		out = append(out, map[string]interface{}{
			"rank":         rank,
			"sourcePod":    sourcePod,
			"sourcePodUID": stringField(pod, "sourcePodUID"),
			"sourceNode":   stringField(pod, "sourceNode"),
			"targetPod":    targetPod,
			"targetNode":   targetNode,
			"archives":     archives[rank],
		})
	}
	if len(out) != len(spec.TargetRanks) {
		return nil, fmt.Errorf("RestoreRequest pods missing target rank mapping")
	}
	return out, nil
}

func partialCheckpointArchiveEvidence(migration *unstructured.Unstructured, spec replacementSpec) (map[int64][]interface{}, error) {
	cluster, ok := sourceClusterStatus(migration, spec.SourceCluster)
	if !ok {
		return nil, fmt.Errorf("partial checkpoint source cluster status required")
	}
	pods, ok := mapSliceFromStatus(cluster, "pods")
	if !ok || len(pods) == 0 {
		return nil, fmt.Errorf("partial checkpoint pod archive evidence required")
	}
	sourceUIDByRank := map[int64]string{}
	for _, item := range spec.Pods {
		pod := item.(map[string]interface{})
		rank := intField(pod, "rank")
		if requiredRank(spec.TargetRanks, rank) {
			sourceUIDByRank[rank] = stringField(pod, "sourcePodUID")
		}
	}
	checkpointID := migration.GetAnnotations()["training.dcnlab.com/checkpoint-id"]
	archives := map[int64][]interface{}{}
	for _, pod := range pods {
		rank := intField(pod, "rank")
		if !requiredRank(spec.TargetRanks, rank) {
			continue
		}
		if phase := stringFromStatus(pod, "phase"); phase != "ContainerCheckpointed" {
			return nil, fmt.Errorf("partial checkpoint rank %d phase must be ContainerCheckpointed", rank)
		}
		podUID := firstString(pod, [][]string{{"sourcePodUID"}, {"podUID"}, {"uid"}})
		if podUID == "" || podUID != sourceUIDByRank[rank] {
			return nil, fmt.Errorf("partial checkpoint rank %d pod UID evidence mismatch", rank)
		}
		podCheckpointID := stringFromStatus(pod, "checkpointID")
		files, ok := mapSliceFromStatus(pod, "checkpointFiles")
		if !ok || len(files) == 0 {
			return nil, fmt.Errorf("partial checkpoint rank %d checkpointFiles required", rank)
		}
		for _, file := range files {
			fileCheckpointID := stringField(file, "checkpointID")
			if checkpointID != "" {
				if podCheckpointID == "" && fileCheckpointID == "" {
					return nil, fmt.Errorf("partial checkpoint rank %d checkpointID binding required", rank)
				}
				if podCheckpointID != "" && podCheckpointID != checkpointID {
					return nil, fmt.Errorf("partial checkpoint rank %d checkpointID mismatch", rank)
				}
				if fileCheckpointID != "" && fileCheckpointID != checkpointID {
					return nil, fmt.Errorf("partial checkpoint rank %d file checkpointID mismatch", rank)
				}
			}
			if stringField(file, "containerName") == "" || stringField(file, "filePath") == "" || stringField(file, "sha256") == "" || stringField(file, "durableRef") == "" {
				return nil, fmt.Errorf("partial checkpoint rank %d archive fields incomplete", rank)
			}
			sha := stringField(file, "sha256")
			archive := map[string]interface{}{
				"containerName": stringField(file, "containerName"),
				"sourcePath":    stringField(file, "filePath"),
				"targetPath":    "/var/lib/kubelet/checkpoints/" + sha + ".tar",
				"sha256":        sha,
				"durableRef":    stringField(file, "durableRef"),
			}
			if fileCheckpointID != "" {
				archive["checkpointID"] = fileCheckpointID
			}
			archives[rank] = append(archives[rank], archive)
		}
	}
	for _, rank := range spec.TargetRanks {
		if len(archives[rank]) == 0 {
			return nil, fmt.Errorf("partial checkpoint evidence missing rank %d", rank)
		}
	}
	return archives, nil
}

func restoreRequestSurvivors(spec replacementSpec, migration *unstructured.Unstructured) ([]interface{}, error) {
	if len(spec.PreservedSurvivors) == 0 {
		return nil, nil
	}
	evidenceByUID := map[string]map[string]interface{}{}
	clusters := nestedClusterStatuses(migration.Object)
	for _, cluster := range clusters {
		pods, _ := mapSliceFromStatus(cluster, "pods")
		for _, pod := range pods {
			uid := firstString(pod, [][]string{{"podUID"}, {"uid"}})
			if uid == "" {
				continue
			}
			if phase := stringField(pod, "phase"); phase != "SurvivorPaused" {
				continue
			}
			if evidence, ok, _ := unstructured.NestedMap(pod, "survivorEvidence"); ok {
				evidenceByUID[uid] = evidence
			}
		}
	}
	out := make([]interface{}, 0, len(spec.PreservedSurvivors))
	for _, item := range spec.PreservedSurvivors {
		base := item.(map[string]interface{})
		uid := stringField(base, "podUID")
		evidence := evidenceByUID[uid]
		if evidence == nil || intField(evidence, "generation") <= 0 || stringField(evidence, "pauseLockPath") == "" {
			return nil, fmt.Errorf("survivor pause evidence missing for pod UID %s", uid)
		}
		merged := deepCopyMap(base)
		merged["generation"] = intField(evidence, "generation")
		merged["pauseLockPath"] = stringField(evidence, "pauseLockPath")
		if observedAt := stringField(evidence, "observedAt"); observedAt != "" {
			merged["observedAt"] = observedAt
		}
		out = append(out, merged)
	}
	return out, nil
}

func replacementRecoverySpec(spec replacementSpec, runtimeObj, restore, migration, oldNP, replacement *unstructured.Unstructured) recoverySpec {
	return recoverySpec{
		PolicyName:                   spec.PolicyName,
		PolicyUID:                    spec.PolicyUID,
		PolicyGeneration:             spec.PolicyGeneration,
		RequestUID:                   string(restore.GetUID()),
		CheckpointID:                 migration.GetAnnotations()["training.dcnlab.com/checkpoint-id"],
		EventID:                      stringField(oldNP.Object, "status", "spot", "eventID"),
		EmergencyEventID:             spec.EmergencyEventID,
		OldMarketType:                stringField(oldNP.Object, "spec", "marketType"),
		DesiredMarketType:            spec.DesiredMarketType,
		Operation:                    spec.Operation,
		SourceCluster:                spec.SourceCluster,
		TargetCluster:                spec.TargetCluster,
		WorkloadUID:                  spec.WorkloadUID,
		TrainingRuntimeName:          runtimeObj.GetName(),
		TrainingRuntimeUID:           string(runtimeObj.GetUID()),
		RestoreRequestName:           restore.GetName(),
		RestoreRequestUID:            string(restore.GetUID()),
		RestoreRequestGeneration:     restore.GetGeneration(),
		OldNodeProvisionName:         spec.OldNodeProvisionName,
		OldNodeProvisionUID:          spec.OldNodeProvisionUID,
		ReplacementNodeProvisionName: spec.ReplacementNodeProvisionName,
		ReplacementNodeProvisionUID:  string(replacement.GetUID()),
	}
}

func (r *ReplacementReconciler) ensureSpotRecovery(ctx context.Context, ns string, replacement replacementSpec, spec recoverySpec) (*unstructured.Unstructured, bool, error) {
	name := spec.Operation + "-cleanup"
	existing := trainingpolicy.NewObject("SpotRecovery")
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, existing)
	if err == nil {
		if existing.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID {
			return nil, false, fmt.Errorf("existing SpotRecovery ownership mismatch")
		}
		return existing, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	desired := trainingpolicy.NewObject("SpotRecovery")
	desired.SetNamespace(ns)
	desired.SetName(name)
	desired.SetLabels(map[string]string{
		trainingpolicy.LabelManagedBy: "hybridspotvm-system",
		trainingpolicy.LabelPolicy:    spec.PolicyName,
		trainingpolicy.LabelPolicyUID: spec.PolicyUID,
		trainingpolicy.LabelRole:      "replacement-cleanup",
	})
	desiredSpec := map[string]interface{}{
		"policyRef":                   map[string]interface{}{"name": spec.PolicyName, "uid": spec.PolicyUID, "generation": spec.PolicyGeneration},
		"requestUID":                  spec.RequestUID,
		"operation":                   spec.Operation,
		"sourceCluster":               spec.SourceCluster,
		"targetCluster":               spec.TargetCluster,
		"workloadRef":                 map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": replacement.WorkloadName, "uid": spec.WorkloadUID},
		"trainingRuntimeRef":          map[string]interface{}{"name": spec.TrainingRuntimeName, "uid": spec.TrainingRuntimeUID},
		"checkpointID":                spec.CheckpointID,
		"oldMarketType":               spec.OldMarketType,
		"desiredMarketType":           spec.DesiredMarketType,
		"restoreRequestRef":           map[string]interface{}{"name": spec.RestoreRequestName, "uid": spec.RestoreRequestUID, "generation": spec.RestoreRequestGeneration},
		"oldNodeProvisionRef":         map[string]interface{}{"name": spec.OldNodeProvisionName, "uid": spec.OldNodeProvisionUID},
		"replacementNodeProvisionRef": map[string]interface{}{"name": spec.ReplacementNodeProvisionName, "uid": spec.ReplacementNodeProvisionUID},
	}
	if spec.EventID != "" {
		desiredSpec["eventID"] = spec.EventID
	}
	if spec.EmergencyEventID != "" {
		desiredSpec["emergencyEventID"] = spec.EmergencyEventID
	}
	desired.Object["spec"] = desiredSpec
	if err := r.Create(ctx, desired); err != nil {
		return nil, false, err
	}
	return desired, true, nil
}

func (r *ReplacementReconciler) createIfMissing(ctx context.Context, desired client.Object) error {
	existing := desired.DeepCopyObject().(client.Object)
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err == nil {
		ownerUID := existing.GetLabels()[trainingpolicy.LabelPolicyUID]
		desiredUID := desired.GetLabels()[trainingpolicy.LabelPolicyUID]
		if ownerUID == "" || ownerUID != desiredUID {
			return fmt.Errorf("existing %T %s/%s owner uid %q does not match desired policy uid %q", desired, desired.GetNamespace(), desired.GetName(), ownerUID, desiredUID)
		}
		if desired.GetObjectKind().GroupVersionKind().Kind == "PropagationPolicy" {
			existingObj, existingOK := existing.(*unstructured.Unstructured)
			desiredObj, desiredOK := desired.(*unstructured.Unstructured)
			if existingOK && desiredOK && !samePlacementSpec(existingObj, desiredObj) {
				return fmt.Errorf("existing PropagationPolicy %s/%s selector or placement drift requires manual intervention", desired.GetNamespace(), desired.GetName())
			}
		}
	}
	return err
}

func (r *ReplacementReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return defaultClock()
}

func patchReplacementStatus(ctx context.Context, c client.Client, obj *unstructured.Unstructured, status map[string]interface{}) error {
	base := obj.DeepCopy()
	obj.Object["status"] = status
	return c.Status().Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *ReplacementReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func int64Value(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed), true
		}
	}
	return 0, false
}

func int64SliceToInterface(values []int64) []interface{} {
	out := make([]interface{}, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func requiredRank(ranks []int64, rank int64) bool {
	for _, value := range ranks {
		if value == rank {
			return true
		}
	}
	return false
}

func deepCopyMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
