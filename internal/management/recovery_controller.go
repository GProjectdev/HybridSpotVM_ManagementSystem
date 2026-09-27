package management

import (
	"context"
	"fmt"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	awsNodeProvisionCluster     = "aws"
	phaseCleanupRequested       = "CleanupRequested"
	spotInterruptionNotice      = "InterruptionNotice"
	spotRebalanceRecommendation = "RebalanceRecommendation"
)

var restoreRequestGVK = schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "RestoreRequest"}

type RecoveryReconciler struct {
	client.Client
	APIReader client.Reader
	Clock     func() time.Time
}

func (r *RecoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("spot-recovery-management").
		For(trainingpolicy.NewObject("SpotRecovery")).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}

type recoverySpec struct {
	PolicyName                   string
	PolicyUID                    string
	PolicyGeneration             int64
	RequestUID                   string
	CheckpointID                 string
	EventID                      string
	Operation                    string
	SourceCluster                string
	TargetCluster                string
	WorkloadUID                  string
	TrainingRuntimeName          string
	TrainingRuntimeUID           string
	RestoreRequestName           string
	RestoreRequestUID            string
	RestoreRequestGeneration     int64
	OldNodeProvisionName         string
	OldNodeProvisionUID          string
	ReplacementNodeProvisionName string
	ReplacementNodeProvisionUID  string
}

func (r *RecoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	recovery := trainingpolicy.NewObject("SpotRecovery")
	if err := r.Get(ctx, req.NamespacedName, recovery); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !recovery.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	phase, err := r.recover(ctx, recovery)
	status := map[string]interface{}{
		"observedGeneration":         recovery.GetGeneration(),
		"phase":                      phase,
		"verifiedAt":                 r.now().UTC().Format(time.RFC3339),
		"silentGenerationAutoDelete": false,
	}
	if stringField(recovery.Object, "spec", "replacementNodeProvisionRef", "name") != "" {
		status["replacementMode"] = "explicit-uid-bound-ref"
	} else {
		status["replacementMode"] = "restore-verified-only"
	}
	if err != nil {
		status["message"] = err.Error()
	}
	if checkpoint := stringField(recovery.Object, "spec", "checkpointID"); phase == "Completed" && checkpoint != "" {
		status["verifiedCheckpointID"] = checkpoint
	}
	if patchErr := patchRecoveryStatus(ctx, r.Client, recovery, status); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	if err != nil {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

func (r *RecoveryReconciler) recover(ctx context.Context, recovery *unstructured.Unstructured) (string, error) {
	spec := readRecoverySpec(recovery)
	if err := spec.validateStaticContract(); err != nil {
		return "Rejected", err
	}
	if err := r.verifyPolicy(ctx, recovery.GetNamespace(), spec); err != nil {
		return "Rejected", err
	}
	if err := r.verifyRuntime(ctx, recovery.GetNamespace(), spec); err != nil {
		return "Rejected", err
	}
	if err := r.verifySpotReplacement(ctx, recovery.GetNamespace(), spec); err != nil {
		return "Rejected", err
	}
	freshRecovery, err := r.freshSpotRecovery(ctx, recovery)
	if err != nil {
		return "Pending", err
	}
	restoreEvidence, err := r.verifyRestoreRequest(ctx, freshRecovery.GetNamespace(), spec)
	if err != nil {
		return "Pending", err
	}
	phase, err := r.verifyAndDeleteNodeProvision(ctx, freshRecovery, spec, restoreEvidence)
	if err != nil {
		return phase, err
	}
	return phase, nil
}

func (r *RecoveryReconciler) freshSpotRecovery(ctx context.Context, cached *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	fresh := trainingpolicy.NewObject("SpotRecovery")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: cached.GetNamespace(), Name: cached.GetName()}, fresh); err != nil {
		return nil, fmt.Errorf("fresh get SpotRecovery: %w", err)
	}
	if fresh.GetUID() != cached.GetUID() || fresh.GetGeneration() != cached.GetGeneration() {
		return nil, fmt.Errorf("SpotRecovery uid/generation changed before delete gate")
	}
	if !fresh.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("SpotRecovery is deleting before delete gate")
	}
	return fresh, nil
}

func readRecoverySpec(obj *unstructured.Unstructured) recoverySpec {
	return recoverySpec{
		PolicyName:                   stringField(obj.Object, "spec", "policyRef", "name"),
		PolicyUID:                    stringField(obj.Object, "spec", "policyRef", "uid"),
		PolicyGeneration:             intField(obj.Object, "spec", "policyRef", "generation"),
		RequestUID:                   stringField(obj.Object, "spec", "requestUID"),
		CheckpointID:                 stringField(obj.Object, "spec", "checkpointID"),
		EventID:                      stringField(obj.Object, "spec", "eventID"),
		Operation:                    stringField(obj.Object, "spec", "operation"),
		SourceCluster:                stringField(obj.Object, "spec", "sourceCluster"),
		TargetCluster:                stringField(obj.Object, "spec", "targetCluster"),
		WorkloadUID:                  stringField(obj.Object, "spec", "workloadRef", "uid"),
		TrainingRuntimeName:          stringField(obj.Object, "spec", "trainingRuntimeRef", "name"),
		TrainingRuntimeUID:           stringField(obj.Object, "spec", "trainingRuntimeRef", "uid"),
		RestoreRequestName:           stringField(obj.Object, "spec", "restoreRequestRef", "name"),
		RestoreRequestUID:            stringField(obj.Object, "spec", "restoreRequestRef", "uid"),
		RestoreRequestGeneration:     intField(obj.Object, "spec", "restoreRequestRef", "generation"),
		OldNodeProvisionName:         stringField(obj.Object, "spec", "oldNodeProvisionRef", "name"),
		OldNodeProvisionUID:          stringField(obj.Object, "spec", "oldNodeProvisionRef", "uid"),
		ReplacementNodeProvisionName: stringField(obj.Object, "spec", "replacementNodeProvisionRef", "name"),
		ReplacementNodeProvisionUID:  stringField(obj.Object, "spec", "replacementNodeProvisionRef", "uid"),
	}
}

func (s recoverySpec) validateStaticContract() error {
	if s.PolicyName == "" || s.PolicyUID == "" || s.PolicyGeneration <= 0 {
		return fmt.Errorf("policyRef name, uid, and generation are required")
	}
	if s.RequestUID == "" || s.RestoreRequestName == "" || s.RestoreRequestUID == "" || s.RestoreRequestGeneration <= 0 {
		return fmt.Errorf("requestUID and restoreRequestRef name/uid/generation are required")
	}
	if s.RequestUID != s.RestoreRequestUID {
		return fmt.Errorf("requestUID must match restoreRequestRef.uid")
	}
	if s.CheckpointID == "" || s.EventID == "" || s.WorkloadUID == "" || s.TrainingRuntimeName == "" || s.TrainingRuntimeUID == "" {
		return fmt.Errorf("checkpointID, eventID, workloadRef.uid, and trainingRuntimeRef name/uid are required")
	}
	if s.Operation == "" || s.SourceCluster == "" || s.TargetCluster == "" {
		return fmt.Errorf("operation, sourceCluster, and targetCluster are required")
	}
	if s.SourceCluster != awsNodeProvisionCluster {
		return fmt.Errorf("sourceCluster must be aws for SpotRecovery node deletion")
	}
	if s.SourceCluster == s.TargetCluster && !s.sameClusterReplacement() {
		return fmt.Errorf("same-cluster restore is allowed only for explicit replacement operations")
	}
	if s.OldNodeProvisionName == "" || s.OldNodeProvisionUID == "" {
		return fmt.Errorf("old NodeProvision name/uid ref is required")
	}
	if (s.ReplacementNodeProvisionName == "") != (s.ReplacementNodeProvisionUID == "") {
		return fmt.Errorf("replacement NodeProvision ref must include both name and uid when provided")
	}
	if s.ReplacementNodeProvisionUID != "" && (s.OldNodeProvisionUID == s.ReplacementNodeProvisionUID || s.OldNodeProvisionName == s.ReplacementNodeProvisionName) {
		return fmt.Errorf("old and replacement NodeProvision must not be the same node")
	}
	return nil
}

func (s recoverySpec) sameClusterReplacement() bool {
	return s.SourceCluster == s.TargetCluster && s.ReplacementNodeProvisionName != "" && s.ReplacementNodeProvisionUID != ""
}

func (r *RecoveryReconciler) verifyPolicy(ctx context.Context, ns string, spec recoverySpec) error {
	policy := trainingpolicy.NewObject("TrainingPolicy")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.PolicyName}, policy); err != nil {
		return fmt.Errorf("get TrainingPolicy: %w", err)
	}
	if !policy.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("TrainingPolicy is deleting")
	}
	if string(policy.GetUID()) != spec.PolicyUID || policy.GetGeneration() != spec.PolicyGeneration {
		return fmt.Errorf("policyRef does not match current TrainingPolicy uid/generation")
	}
	if stringField(policy.Object, "spec", "workloadRef", "uid") != spec.WorkloadUID {
		return fmt.Errorf("TrainingPolicy workloadRef.uid mismatch")
	}
	return nil
}

func (r *RecoveryReconciler) verifyRuntime(ctx context.Context, ns string, spec recoverySpec) error {
	runtime := trainingpolicy.NewObject("TrainingRuntime")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.TrainingRuntimeName}, runtime); err != nil {
		return fmt.Errorf("get TrainingRuntime: %w", err)
	}
	if !runtime.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("TrainingRuntime is deleting")
	}
	if string(runtime.GetUID()) != spec.TrainingRuntimeUID {
		return fmt.Errorf("trainingRuntimeRef uid mismatch")
	}
	if stringField(runtime.Object, "spec", "workloadRef", "uid") != spec.WorkloadUID {
		return fmt.Errorf("TrainingRuntime workloadRef.uid mismatch")
	}
	return nil
}

func (r *RecoveryReconciler) verifySpotReplacement(ctx context.Context, ns string, spec recoverySpec) error {
	if spec.ReplacementNodeProvisionName == "" {
		return nil
	}
	replacement := newSpotReplacementObject()
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.Operation}, replacement); err != nil {
		return fmt.Errorf("get SpotReplacement operation: %w", err)
	}
	if !replacement.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("SpotReplacement operation is deleting")
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"policyRef.uid", stringField(replacement.Object, "spec", "policyRef", "uid"), spec.PolicyUID},
		{"operation", stringField(replacement.Object, "spec", "operation"), spec.Operation},
		{"sourceCluster", stringField(replacement.Object, "spec", "sourceCluster"), spec.SourceCluster},
		{"targetCluster", stringField(replacement.Object, "spec", "targetCluster"), spec.TargetCluster},
		{"oldNodeProvisionRef.name", stringField(replacement.Object, "spec", "oldNodeProvisionRef", "name"), spec.OldNodeProvisionName},
		{"oldNodeProvisionRef.uid", stringField(replacement.Object, "spec", "oldNodeProvisionRef", "uid"), spec.OldNodeProvisionUID},
		{"replacementNodeProvisionRef.name", stringField(replacement.Object, "spec", "replacementNodeProvisionRef", "name"), spec.ReplacementNodeProvisionName},
		{"desiredMarketType", stringField(replacement.Object, "spec", "desiredMarketType"), "OnDemand"},
	}
	for _, check := range checks {
		if check.got == "" || check.got != check.want {
			return fmt.Errorf("SpotReplacement %s mismatch", check.name)
		}
	}
	if uid := stringField(replacement.Object, "status", "replacementNodeProvisionRef", "uid"); uid != "" && uid != spec.ReplacementNodeProvisionUID {
		return fmt.Errorf("SpotReplacement replacementNodeProvisionRef uid mismatch")
	}
	targetRanks, ok, _ := unstructured.NestedSlice(replacement.Object, "spec", "partialCheckpoint", "targetRanks")
	if !ok || len(targetRanks) == 0 {
		return fmt.Errorf("SpotReplacement partialCheckpoint.targetRanks required")
	}
	ranks := map[int64]bool{}
	for _, item := range targetRanks {
		rank, ok := int64Value(item)
		if !ok || rank < 0 {
			return fmt.Errorf("SpotReplacement partialCheckpoint target rank invalid")
		}
		ranks[rank] = true
	}
	pods, ok, _ := unstructured.NestedSlice(replacement.Object, "spec", "pods")
	if !ok || len(pods) == 0 {
		return fmt.Errorf("SpotReplacement pods rank/sourcePodUID evidence required")
	}
	seenPods := map[int64]bool{}
	for _, item := range pods {
		pod, ok := item.(map[string]interface{})
		rank := intField(pod, "rank")
		if !ok || !ranks[rank] || stringField(pod, "sourcePodUID") == "" {
			return fmt.Errorf("SpotReplacement pods rank/sourcePodUID evidence incomplete")
		}
		seenPods[rank] = true
	}
	for rank := range ranks {
		if !seenPods[rank] {
			return fmt.Errorf("SpotReplacement missing pod evidence for target rank")
		}
	}
	if !boolField(replacement.Object, "spec", "partialRestore", "preventPeriodicResume") {
		return fmt.Errorf("SpotReplacement partialRestore.preventPeriodicResume required")
	}
	if partialRanks, ok, _ := unstructured.NestedSlice(replacement.Object, "spec", "partialRestore", "targetRanks"); ok && len(partialRanks) > 0 {
		if len(partialRanks) != len(targetRanks) {
			return fmt.Errorf("SpotReplacement partialRestore.targetRanks must match partialCheckpoint.targetRanks")
		}
	}
	return nil
}

type restoreEvidence struct {
	SourceNodes map[string]bool
}

func (r *RecoveryReconciler) verifyRestoreRequest(ctx context.Context, ns string, spec recoverySpec) (restoreEvidence, error) {
	req := newRestoreRequest()
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.RestoreRequestName}, req); err != nil {
		return restoreEvidence{}, fmt.Errorf("get RestoreRequest: %w", err)
	}
	if string(req.GetUID()) != spec.RestoreRequestUID || req.GetGeneration() != spec.RestoreRequestGeneration {
		return restoreEvidence{}, fmt.Errorf("RestoreRequest uid/generation mismatch")
	}
	if !req.GetDeletionTimestamp().IsZero() {
		return restoreEvidence{}, fmt.Errorf("RestoreRequest is deleting")
	}
	if stringField(req.Object, "status", "phase") != "Verified" {
		return restoreEvidence{}, fmt.Errorf("RestoreRequest verification is not Verified")
	}
	if intField(req.Object, "status", "observedGeneration") != spec.RestoreRequestGeneration {
		return restoreEvidence{}, fmt.Errorf("RestoreRequest observedGeneration mismatch")
	}
	if err := verifyRestoreEvidenceContract(req, spec); err != nil {
		return restoreEvidence{}, err
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"requestUID", stringField(req.Object, "status", "verification", "requestUID"), spec.RequestUID},
		{"checkpointID", stringField(req.Object, "status", "verification", "checkpointID"), spec.CheckpointID},
		{"sourceCluster", stringField(req.Object, "status", "verification", "sourceCluster"), spec.SourceCluster},
		{"targetCluster", stringField(req.Object, "status", "verification", "targetCluster"), spec.TargetCluster},
		{"workloadUID", stringField(req.Object, "spec", "workloadRef", "uid"), spec.WorkloadUID},
		{"spec.source", firstString(req.Object, [][]string{{"spec", "sourceCluster"}, {"spec", "source"}}), spec.SourceCluster},
		{"spec.target", firstString(req.Object, [][]string{{"spec", "targetCluster"}, {"spec", "target"}}), spec.TargetCluster},
		{"checkpointRef.checkpointID", stringField(req.Object, "spec", "checkpointRef", "checkpointID"), spec.CheckpointID},
		{"trainingRuntimeRef.name", stringField(req.Object, "status", "verification", "trainingRuntimeRef", "name"), spec.TrainingRuntimeName},
		{"trainingRuntimeRef.uid", stringField(req.Object, "status", "verification", "trainingRuntimeRef", "uid"), spec.TrainingRuntimeUID},
	}
	for _, check := range checks {
		if check.got == "" || check.got != check.want {
			return restoreEvidence{}, fmt.Errorf("RestoreRequest verification %s mismatch", check.name)
		}
	}
	if stringField(req.Object, "status", "verification", "verifiedAt") == "" {
		return restoreEvidence{}, fmt.Errorf("RestoreRequest verification verifiedAt required")
	}
	sourceNodes := map[string]bool{}
	pods, ok, _ := unstructured.NestedSlice(req.Object, "spec", "pods")
	if !ok || len(pods) == 0 {
		return restoreEvidence{}, fmt.Errorf("RestoreRequest spec.pods sourceNode evidence required")
	}
	for _, item := range pods {
		pod, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if node := stringField(pod, "sourceNode"); node != "" {
			sourceNodes[node] = true
		}
	}
	if len(sourceNodes) == 0 {
		return restoreEvidence{}, fmt.Errorf("RestoreRequest spec.pods sourceNode evidence required")
	}
	return restoreEvidence{SourceNodes: sourceNodes}, nil
}

func verifyRestoreEvidenceContract(req *unstructured.Unstructured, spec recoverySpec) error {
	if spec.Operation != "" && stringField(req.Object, "status", "verification", "operation") != spec.Operation {
		return fmt.Errorf("RestoreRequest verification operation mismatch")
	}
	if spec.sameClusterReplacement() {
		fence, ok, _ := unstructured.NestedMap(req.Object, "status", "verification", "sourceFence")
		if !ok || !boolField(map[string]interface{}{"sourceFence": fence}, "sourceFence", "fenced") || stringField(fence, "evidenceID") == "" || stringField(fence, "observedAt") == "" {
			return fmt.Errorf("RestoreRequest sourceFence durable evidence required")
		}
		if spec.Operation != "" && stringField(fence, "operation") != spec.Operation {
			return fmt.Errorf("RestoreRequest sourceFence operation mismatch")
		}
		if boolField(req.Object, "spec", "sourceFenced") && stringField(fence, "evidenceID") == "" {
			return fmt.Errorf("RestoreRequest sourceFenced boolean cannot replace sourceFence evidence")
		}
		if boolField(req.Object, "spec", "sourceFenced") {
			return fmt.Errorf("same-cluster replacement requires sourceFenced=false until member actuator evidence is bound")
		}
		if !boolField(req.Object, "spec", "volumesReady") {
			return fmt.Errorf("same-cluster replacement volumesReady evidence required")
		}
		if !boolField(req.Object, "spec", "partialRestore", "preventPeriodicResume") && !boolField(req.Object, "status", "verification", "partialRestore", "preventPeriodicResume") {
			return fmt.Errorf("RestoreRequest partialRestore.preventPeriodicResume contract evidence required")
		}
		targetRanks, ok, _ := unstructured.NestedSlice(req.Object, "status", "verification", "partialRestore", "targetRanks")
		if !ok || len(targetRanks) == 0 {
			return fmt.Errorf("RestoreRequest partialRestore target rank evidence required")
		}
		for _, item := range targetRanks {
			rank, ok := item.(map[string]interface{})
			if !ok || intField(rank, "rank") < 0 || stringField(rank, "targetPodUID") == "" || stringField(rank, "checkpointID") != spec.CheckpointID || (stringField(rank, "archiveEvidenceID") == "" && (stringField(rank, "durableRef") == "" || stringField(rank, "sha256") == "")) {
				return fmt.Errorf("RestoreRequest partialRestore target rank evidence incomplete")
			}
		}
		survivors, ok, _ := unstructured.NestedSlice(req.Object, "status", "verification", "survivors")
		if !ok || len(survivors) == 0 {
			return fmt.Errorf("RestoreRequest survivor UID preservation evidence required")
		}
		for _, item := range survivors {
			survivor, ok := item.(map[string]interface{})
			if !ok || intField(survivor, "rank") < 0 || stringField(survivor, "podUID") == "" || stringField(survivor, "stateEvidence", "kind") == "" || stringField(survivor, "stateEvidence", "observedAt") == "" {
				return fmt.Errorf("RestoreRequest survivor UID preservation evidence incomplete")
			}
		}
	}
	return nil
}

func (r *RecoveryReconciler) verifyAndDeleteNodeProvision(ctx context.Context, recovery *unstructured.Unstructured, spec recoverySpec, restore restoreEvidence) (string, error) {
	ns := recovery.GetNamespace()
	if err := r.verifyRetirementMarkerPersisted(ctx, recovery, spec); err != nil {
		return "Pending", err
	}
	oldNP, err := r.getNodeProvision(ctx, ns, spec.OldNodeProvisionName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "Completed", nil
		}
		return "Pending", err
	}
	if !oldNP.GetDeletionTimestamp().IsZero() {
		return phaseCleanupRequested, fmt.Errorf("old NodeProvision deletion is still pending")
	}
	if string(oldNP.GetUID()) != spec.OldNodeProvisionUID {
		return "Pending", fmt.Errorf("old NodeProvision uid mismatch")
	}
	if oldNP.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID {
		return "Pending", fmt.Errorf("old NodeProvision policy uid mismatch")
	}
	if err := verifyOldNodeRIC(oldNP, spec, restore); err != nil {
		return "Pending", err
	}

	if spec.ReplacementNodeProvisionName != "" {
		replacement, err := r.getNodeProvision(ctx, ns, spec.ReplacementNodeProvisionName)
		if err != nil {
			return "Pending", err
		}
		if !replacement.GetDeletionTimestamp().IsZero() {
			return "Pending", fmt.Errorf("replacement NodeProvision is deleting")
		}
		if string(replacement.GetUID()) != spec.ReplacementNodeProvisionUID {
			return "Pending", fmt.Errorf("replacement NodeProvision uid mismatch")
		}
		if err := verifyReplacementReady(replacement, spec); err != nil {
			return "Pending", err
		}
	}

	uid := types.UID(spec.OldNodeProvisionUID)
	rv := oldNP.GetResourceVersion()
	if err := r.Delete(ctx, oldNP, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil {
		return "Pending", err
	}
	return phaseCleanupRequested, fmt.Errorf("old NodeProvision deletion requested; waiting for confirmed NotFound")
}

func (r *RecoveryReconciler) verifyRetirementMarkerPersisted(ctx context.Context, recovery *unstructured.Unstructured, spec recoverySpec) error {
	list := trainingpolicy.NewList("SpotRecovery")
	if err := r.reader().List(ctx, list, client.InNamespace(recovery.GetNamespace())); err != nil {
		return fmt.Errorf("list SpotRecovery retired slots: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetUID() != recovery.GetUID() {
			continue
		}
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if phase == "Rejected" {
			return fmt.Errorf("SpotRecovery retirement marker is rejected")
		}
		policyUID, _, _ := unstructured.NestedString(item.Object, "spec", "policyRef", "uid")
		name, _, _ := unstructured.NestedString(item.Object, "spec", "oldNodeProvisionRef", "name")
		uid, _, _ := unstructured.NestedString(item.Object, "spec", "oldNodeProvisionRef", "uid")
		if policyUID == spec.PolicyUID && name == spec.OldNodeProvisionName && uid == spec.OldNodeProvisionUID {
			return nil
		}
		return fmt.Errorf("SpotRecovery retirement marker does not match old NodeProvision")
	}
	return fmt.Errorf("SpotRecovery retirement marker is not visible through APIReader")
}

func (r *RecoveryReconciler) getNodeProvision(ctx context.Context, ns, name string) (*unstructured.Unstructured, error) {
	np := trainingpolicy.NewObject("NodeProvision")
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, np); err != nil {
		return nil, err
	}
	return np, nil
}

func verifyOldNodeRIC(np *unstructured.Unstructured, spec recoverySpec, restore restoreEvidence) error {
	if observed := stringField(np.Object, "status", "observedCluster"); observed != spec.SourceCluster {
		return fmt.Errorf("old NodeProvision observedCluster must match sourceCluster")
	}
	spot, _, _ := unstructured.NestedMap(np.Object, "status", "spot")
	if spot == nil {
		return fmt.Errorf("old NodeProvision status.spot RIC evidence required")
	}
	instanceID := stringField(np.Object, "status", "instanceId")
	atRisk, _, _ := unstructured.NestedBool(map[string]interface{}{"spot": spot}, "spot", "atRisk")
	if stringField(spot, "eventID") != spec.EventID || instanceID == "" || stringField(spot, "instanceID") != instanceID || !atRisk || !validSpotSignalType(stringField(spot, "signalType")) {
		return fmt.Errorf("old NodeProvision eventID, instanceID, atRisk, and signalType evidence required")
	}
	nodeName := firstString(np.Object, [][]string{{"status", "nodeName"}, {"spec", "nodeName"}, {"spec", "hostname"}})
	if nodeName == "" || !restore.SourceNodes[nodeName] {
		return fmt.Errorf("old NodeProvision nodeName is not a verified RestoreRequest sourceNode")
	}
	if op := np.GetAnnotations()["training.dcnlab.com/recovery-operation"]; op != "" && op != spec.Operation {
		return fmt.Errorf("old NodeProvision operation mismatch")
	}
	return nil
}

func validSpotSignalType(signalType string) bool {
	return signalType == spotInterruptionNotice || signalType == spotRebalanceRecommendation
}

func verifyReplacementReady(np *unstructured.Unstructured, spec recoverySpec) error {
	if np.GetLabels()[trainingpolicy.LabelPolicyUID] != spec.PolicyUID {
		return fmt.Errorf("replacement NodeProvision policy uid mismatch")
	}
	if market := stringField(np.Object, "spec", "marketType"); market != "OnDemand" {
		return fmt.Errorf("replacement NodeProvision marketType must be OnDemand")
	}
	if phase := stringField(np.Object, "status", "phase"); phase != "Ready" {
		return fmt.Errorf("replacement NodeProvision is not Ready")
	}
	if stringField(np.Object, "status", "instanceId") == "" {
		return fmt.Errorf("replacement NodeProvision instanceId evidence required")
	}
	if op := np.GetAnnotations()["training.dcnlab.com/recovery-operation"]; op != spec.Operation {
		return fmt.Errorf("replacement NodeProvision operation mismatch")
	}
	return nil
}

func newRestoreRequest() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(restoreRequestGVK)
	return obj
}

func patchRecoveryStatus(ctx context.Context, c client.Client, obj *unstructured.Unstructured, status map[string]interface{}) error {
	base := obj.DeepCopy()
	obj.Object["status"] = status
	return c.Status().Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *RecoveryReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return defaultClock()
}

func (r *RecoveryReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func stringField(obj map[string]interface{}, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj, fields...)
	return value
}

func intField(obj map[string]interface{}, fields ...string) int64 {
	if value, ok, _ := unstructured.NestedInt64(obj, fields...); ok {
		return value
	}
	if value, ok, _ := unstructured.NestedFloat64(obj, fields...); ok {
		return int64(value)
	}
	return 0
}

func boolField(obj map[string]interface{}, fields ...string) bool {
	value, _, _ := unstructured.NestedBool(obj, fields...)
	return value
}

func firstString(obj map[string]interface{}, paths [][]string) string {
	for _, path := range paths {
		if value := stringField(obj, path...); value != "" {
			return value
		}
	}
	return ""
}
