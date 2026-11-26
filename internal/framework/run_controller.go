package framework

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// RunReconciler owns only TrainingRun status, placement and runtime registration.
// It never calls an EC2 API or treats a Running Pod as training progress.
type RunReconciler struct {
	client.Client
	Members              MemberReader
	Now                  func() time.Time
	GateImage            string
	GateCredentialSecret string
}

func (r *RunReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *RunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	run := named("TrainingRun", req.Namespace, req.Name)
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	status, _, _ := unstructured.NestedMap(run.Object, "status")
	if status == nil {
		status = map[string]interface{}{}
	}
	status["observedGeneration"], status["observedAt"] = run.GetGeneration(), stamp(r.now())
	report := func(phase, message string) (ctrl.Result, error) {
		status["phase"], status["message"] = phase, message
		return ctrl.Result{RequeueAfter: 5 * time.Second}, resource.SetStatus(ctx, r.Client, run, status)
	}
	var s RunSpec
	if err := decodeSpec(run, &s); err != nil {
		return report("Invalid", err.Error())
	}
	if err := s.Validate(); err != nil {
		return report("Invalid", err.Error())
	}
	if s.Suspend {
		return report("Suspended", "new control actions suspended; existing workload and capacity retained")
	}
	work := object("apps/v1", "StatefulSet")
	if err := r.Get(ctx, key(req.Namespace, s.WorkloadRef.Name), work); err != nil {
		return report("WaitingWorkload", err.Error())
	}
	if !work.GetDeletionTimestamp().IsZero() || work.GetUID() == "" || (s.WorkloadRef.UID != "" && string(work.GetUID()) != s.WorkloadRef.UID) {
		return report("IdentityConflict", "workload UID mismatch or deletion in progress")
	}
	if previous, ok := status["workloadUID"].(string); ok && previous != string(work.GetUID()) {
		return report("IdentityConflict", "workload was recreated; submit a new TrainingRun")
	}
	if status["workloadUID"] == nil {
		status["workloadUID"] = string(work.GetUID())
		return report("BindingWorkload", "workload UID persisted before any placement write")
	}
	if integer(work, "spec", "replicas") != s.WorldSize {
		return report("Invalid", "StatefulSet replicas must equal worldSize")
	}
	if err := r.exclusiveWorkload(ctx, run, work); err != nil {
		return report("OwnershipConflict", err.Error())
	}

	if s.Mode == "Execute" {
		transition, err := activeTransition(ctx, r.Client, run)
		if err != nil {
			return report("TransitionConflict", err.Error())
		}
		if transition != nil {
			phase, message, e := r.reconcileTransitionIntent(ctx, run, work, transition, s, status)
			if e != nil {
				message = e.Error()
			}
			return report(phase, message)
		}
	}
	committed, _ := status["committedCluster"].(string)
	var capacity *unstructured.Unstructured
	if s.Cloud.Allowed && s.Mode == "Execute" {
		var err error
		capacity, err = r.initialCapacity(ctx, run, s, status, false)
		if err != nil {
			return report("CapacityBlocked", err.Error())
		}
	}
	if s.Mode == "Execute" && committed != "" {
		created, e := r.transitionTrigger(ctx, run, work, capacity, s, WorkloadObservation{}, status)
		if e != nil {
			status["transitionObservationError"] = e.Error()
		}
		if created {
			return report("TransitionRequested", "observed interruption or confirmed failure; immutable recovery intent persisted")
		}
	}
	cloudReady := capacityReady(capacity, s.WorldSize, r.now())
	cloudEligible, _ := status["cloudEligible"].(bool)
	preferredInitialCluster := ""
	if s.Mode == "Execute" {
		if err := r.labelWorkload(ctx, run, work); err != nil {
			return report("OwnershipConflict", err.Error())
		}
		if err := r.prepareExecutionGate(ctx, run, work, s); err != nil {
			return report("ExecutionGateBlocked", err.Error())
		}
		clusters := []string{s.OnPremiseCluster}
		if committed != "" {
			if committed != s.OnPremiseCluster && committed != s.Cloud.Cluster {
				return report("IdentityConflict", "foreign committed cluster")
			}
			clusters = []string{committed}
		} else if cloudEligible && cloudReady {
			if err := ensureCloudNodePlacement(ctx, r.Client, run, work, capacity, s); err != nil {
				return report("CloudPlacementBlocked", err.Error())
			}
			reader, e := r.Members(s.OnPremiseCluster)
			if e != nil {
				return report("PlacementObservationBlocked", e.Error())
			}
			hasRoom, evidence, e := initialGPUHeadroom(ctx, reader, work, s.WorldSize)
			if e != nil {
				return report("PlacementObservationBlocked", e.Error())
			}
			evidence["observedAt"] = stamp(r.now())
			status["onPremGPUHeadroom"] = evidence
			preferredInitialCluster = s.OnPremiseCluster
			if !hasRoom {
				clusters = []string{s.Cloud.Cluster}
				preferredInitialCluster = s.Cloud.Cluster
			}
		}
		if err := ensureWorkloadPlacement(ctx, r.Client, run, work, clusters, committed == ""); err != nil {
			return report("PlacementError", err.Error())
		}
	}
	o, err := ObserveWorkload(ctx, r.Client, r.Members, run, work, s, r.now())
	if err != nil {
		return report("ObservationBlocked", err.Error())
	}
	status["selectedCluster"], status["gpuShortage"], status["allPodsScheduled"] = o.SelectedCluster, o.GPUShortage, o.AllPodsScheduled
	if preferredInitialCluster != "" && o.SelectedCluster != preferredInitialCluster {
		return report("AwaitingNativePlacement", "waiting for Karmada to apply the current pre-admission capacity eligibility")
	}
	if s.Mode == "Execute" {
		requested, e := requestInitialReschedule(ctx, r.Client, run, work, s, o, cloudReady, r.now())
		if e != nil {
			return report("PlacementError", e.Error())
		}
		if requested {
			return report("RecheckingPlacement", "Karmada native rescheduling requested before any execution permit")
		}
	}
	if s.Mode == "Execute" && o.SelectedCluster != "" {
		if o.SelectedCluster != s.OnPremiseCluster && (o.SelectedCluster != s.Cloud.Cluster || (committed == "" && (!cloudEligible || !cloudReady))) {
			return report("UnsafePlacement", "Cloud is not eligible with a verified complete target world")
		}
		if err := r.ensureRuntime(ctx, run, work, s, o.SelectedCluster); err != nil {
			return report("RuntimeError", err.Error())
		}
	}
	admitted := o.AnyPodScheduled
	if s.ExecutionGate {
		admitted = o.AllPodsScheduled
	}
	if s.ExecutionGate && s.Mode == "Execute" && s.Cloud.Allowed && o.SelectedCluster == s.Cloud.Cluster {
		if err := verifyCloudScheduledNodes(ctx, r.Members, activeCapacityView(run, capacity), o, s.Cloud.Cluster); err != nil {
			return report("CloudExecutionBlocked", err.Error())
		}
	}
	if admitted && o.SelectedCluster != "" {
		if committed != "" && committed != o.SelectedCluster {
			return report("UnsafePlacement", "execution member changed without a verified transition")
		}
		committed = o.SelectedCluster
		status["committedCluster"] = committed
	}
	if s.Mode == "Execute" {
		created, e := r.transitionTrigger(ctx, run, work, capacity, s, o, status)
		if e != nil {
			status["transitionObservationError"] = e.Error()
		} else {
			delete(status, "transitionObservationError")
		}
		if created {
			return report("TransitionRequested", "immutable transition intent recorded before replacement capacity changes")
		}
	}
	if s.ExecutionGate && s.Mode == "Execute" {
		if err := initialPermit(status, work, o, stamp(r.now())); err != nil {
			return report("TransitionRequired", err.Error())
		}
	}
	if o.GPUShortage && status["shortageSince"] == nil {
		status["shortageSince"] = stamp(r.now())
	}
	shortageAt, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(status["shortageSince"]))
	w, lead := s.Wait()
	d, err := DecideActivation(ActivationObservation{Now: r.now(), ShortageAt: shortageAt, WaitSeconds: w, PreparationSeconds: lead, CloudAllowed: s.Cloud.Allowed, ResourceShortage: o.GPUShortage, CloudReady: cloudReady, OnPremAdmitted: admitted && o.SelectedCluster == s.OnPremiseCluster, CommittedCluster: committed, OnPremCluster: s.OnPremiseCluster, CloudCluster: s.Cloud.Cluster})
	if err != nil {
		return report("DecisionBlocked", err.Error())
	}
	status["activation"] = map[string]interface{}{"phase": d.Phase, "prepareCloud": d.PrepareCloud, "allowCloud": d.AllowCloud, "retireUnusedCloud": d.RetireUnusedCloud}
	if !d.PrepareAt.IsZero() {
		status["activation"].(map[string]interface{})["prepareAt"] = stamp(d.PrepareAt)
	}
	if s.Mode == "Execute" {
		if d.RetireUnusedCloud {
			if err := retireUnusedCapacity(ctx, r.Client, capacity); err != nil {
				return report("CapacityRetirementBlocked", err.Error())
			}
		}
		if d.PrepareCloud && capacity == nil {
			var err error
			capacity, err = r.initialCapacity(ctx, run, s, status, true)
			if err != nil {
				return report("CloudPolicyBlocked", err.Error())
			}
		}
		if d.AllowCloud {
			status["cloudEligible"] = true
		}
	}
	if o.Running {
		if status["trainingStartedAt"] == nil {
			status["trainingStartedAt"] = stamp(r.now())
		}
		status["commonStep"] = o.Runtime.GlobalStep
		return report("Running", "all ranks have fresh UID-bound training progress")
	}
	if d.PrepareCloud {
		if s.Mode != "Execute" {
			return report("CloudActivationBlocked", "Observe mode records decisions without creating capacity")
		}
		if d.AllowCloud {
			return report("CloudEligible", "complete GPU world ready; Karmada may select Cloud after the requested wait")
		}
		return report("PreparingCloud", "UID-bound capacity requested; On-Premise admission retains priority before commitment")
	}
	if committed != "" {
		return report("AwaitingTrainingProgress", "member committed; waiting for fresh all-rank telemetry")
	}
	return report(d.Phase, "waiting for verified placement and training progress")
}

func (r *RunReconciler) exclusiveWorkload(ctx context.Context, run, work *unstructured.Unstructured) error {
	for _, kind := range []string{"TrainingRun", "NodeRequest"} {
		items, err := listItems(ctx, r.Client, "training.dcnlab.com/v1alpha1", kind, run.GetNamespace())
		if err != nil {
			return err
		}
		for i := range items {
			o := &items[i]
			if o.GetUID() != run.GetUID() && str(o, "spec", "workloadRef", "name") == work.GetName() {
				return fmt.Errorf("workload already referenced by %s/%s", kind, o.GetName())
			}
		}
	}
	items, err := listItems(ctx, r.Client, "policy.karmada.io/v1alpha1", "PropagationPolicy", run.GetNamespace())
	if err != nil {
		return err
	}
	for i := range items {
		o := &items[i]
		if owned(o, run) {
			continue
		}
		selectors, _, _ := unstructured.NestedSlice(o.Object, "spec", "resourceSelectors")
		for _, raw := range selectors {
			v, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if v["apiVersion"] == "apps/v1" && v["kind"] == "StatefulSet" {
				if name, ok := v["name"].(string); !ok || name == "" || name == work.GetName() {
					return fmt.Errorf("foreign placement policy may select workload: %s", o.GetName())
				}
			}
		}
	}
	return nil
}

func (r *RunReconciler) labelWorkload(ctx context.Context, run, work *unstructured.Unstructured) error {
	labels := work.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	if uid := labels[RunUIDLabel]; uid != "" && uid != string(run.GetUID()) {
		return fmt.Errorf("workload owned by another Run")
	}
	if uid := labels["training.dcnlab.com/workload-uid"]; uid != "" && uid != string(work.GetUID()) {
		return fmt.Errorf("workload origin UID conflict")
	}
	before := work.DeepCopy()
	labels[RunUIDLabel] = string(run.GetUID())
	labels["training.dcnlab.com/workload-uid"] = string(work.GetUID())
	work.SetLabels(labels)
	// Metadata labels do not roll a StatefulSet or change its training template.
	if reflect.DeepEqual(before.GetLabels(), labels) {
		return nil
	}
	return r.Patch(ctx, work, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func placementPolicy(obj *unstructured.Unstructured, clusters []string) *unstructured.Unstructured {
	pp := object("policy.karmada.io/v1alpha1", "PropagationPolicy")
	pp.SetNamespace(obj.GetNamespace())
	pp.SetName(obj.GetName() + "-training-placement")
	groups := []interface{}{}
	for i, cluster := range clusters {
		groups = append(groups, map[string]interface{}{"affinityName": fmt.Sprintf("priority-%d", i), "clusterNames": []interface{}{cluster}})
	}
	pp.Object["spec"] = map[string]interface{}{"conflictResolution": "Abort", "resourceSelectors": []interface{}{map[string]interface{}{"apiVersion": obj.GetAPIVersion(), "kind": obj.GetKind(), "name": obj.GetName()}}, "placement": map[string]interface{}{"clusterAffinities": groups, "spreadConstraints": []interface{}{map[string]interface{}{"spreadByField": "cluster", "minGroups": int64(1), "maxGroups": int64(1)}}}}
	return pp
}

func ensurePlacement(ctx context.Context, c client.Client, owner, obj *unstructured.Unstructured, clusters []string) error {
	return ensureSpec(ctx, c, placementPolicy(obj, clusters), owner)
}

func ensureWorkloadPlacement(ctx context.Context, c client.Client, run, work *unstructured.Unstructured, clusters []string, initial bool) error {
	pp := placementPolicy(work, clusters)
	if initial {
		// Duplicated scheduling does not divide according to available resources.
		// Aggregated uses Karmada's native capacity calculation; maxGroups=1 keeps
		// the full DDP world together and ordered affinities retain On-Prem priority.
		_ = unstructured.SetNestedMap(pp.Object, map[string]interface{}{"replicaSchedulingType": "Divided", "replicaDivisionPreference": "Aggregated"}, "spec", "placement", "replicaScheduling")
	}
	return ensureSpec(ctx, c, pp, run)
}

func (r *RunReconciler) ensureRuntime(ctx context.Context, run, work *unstructured.Unstructured, s RunSpec, cluster string) error {
	rt := p.NewObject("TrainingRuntime")
	rt.SetNamespace(run.GetNamespace())
	rt.SetName(run.GetName() + "-runtime")
	port := s.RuntimePort
	if port == 0 {
		port = p.DefaultControlPort
	}
	rt.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": work.GetName(), "uid": string(work.GetUID())}, "sourceCluster": cluster, "expectedWorldSize": s.WorldSize, "port": port}
	if s.Container != "" {
		_ = unstructured.SetNestedField(rt.Object, s.Container, "spec", "container")
	}
	if err := ensureSpec(ctx, r.Client, rt, run); err != nil {
		return err
	}
	return ensurePlacement(ctx, r.Client, run, rt, []string{cluster})
}

func SetupRun(mgr ctrl.Manager) error {
	r := &RunReconciler{Client: mgr.GetClient(), Members: configuredMembers(), GateImage: os.Getenv("TRAINING_GATE_IMAGE"), GateCredentialSecret: os.Getenv("TRAINING_GATE_CREDENTIAL_SECRET")}
	return ctrl.NewControllerManagedBy(mgr).For(resource.Object("TrainingRun")).WithEventFilter(predicate.GenerationChangedPredicate{}).Complete(r)
}
