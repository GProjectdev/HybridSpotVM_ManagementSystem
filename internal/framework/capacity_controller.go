package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const originProvisionUID = "training.dcnlab.com/origin-provision-uid"

// CapacityReconciler translates UID-bound allocation intents into existing
// NodeProvisions. It never chooses a market or invokes a cloud API itself.
// Provider-confirmed fence receipts are retained before deletion is requested.
type CapacityReconciler struct {
	client.Client
	Members MemberReader
	Now     func() time.Time
}

func (r *CapacityReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *CapacityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cap := named("TrainingCapacity", req.Namespace, req.Name)
	if err := r.Get(ctx, req.NamespacedName, cap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status, _, _ := unstructured.NestedMap(cap.Object, "status")
	if status == nil {
		status = map[string]interface{}{}
	}
	receipts, _, _ := unstructured.NestedMap(status, "allocations")
	if receipts == nil {
		receipts = map[string]interface{}{}
	}
	status["allocations"] = receipts
	report := func(phase, msg string) (ctrl.Result, error) {
		status["phase"], status["message"], status["observedAt"], status["observedGeneration"] = phase, msg, stamp(r.now()), cap.GetGeneration()
		err := resource.SetStatus(ctx, r.Client, cap, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	var s CapacitySpec
	if err := decodeSpec(cap, &s); err != nil {
		return report("Invalid", err.Error())
	}
	deadline, err := time.Parse(time.RFC3339, s.Deadline)
	if err != nil || s.RunRef.UID == "" || s.MaxWorkers < 1 || len(s.Allocations) == 0 {
		return report("Invalid", "UID-bound owner, positive worker limit, allocations and absolute deadline required")
	}
	run := named("TrainingRun", req.Namespace, s.RunRef.Name)
	err = r.Get(ctx, key(req.Namespace, s.RunRef.Name), run)
	ownerGone := apierrors.IsNotFound(err) || (err == nil && string(run.GetUID()) != s.RunRef.UID)
	if ownerGone {
		approved, ok := status["approvedRunSpec"].(map[string]interface{})
		if !ok || status["approvedRunUID"] != s.RunRef.UID {
			return report("OwnerUnavailable", "owner missing without a persisted allocation authorization")
		}
		run.SetUID(types.UID(s.RunRef.UID))
		run.Object["spec"] = approved
	} else if err != nil {
		return report("OwnerUnavailable", "retained capacity requires owner inspection; no new allocation")
	}
	var rs RunSpec
	expired := !r.now().Before(deadline)
	deleting := ownerGone || !cap.GetDeletionTimestamp().IsZero() || !run.GetDeletionTimestamp().IsZero()
	authorized := func() bool {
		return decodeSpec(run, &rs) == nil && string(run.GetUID()) == s.RunRef.UID && owned(cap, run) && rs.Mode == "Execute" && rs.Cloud.Allowed && rs.ExecutionGate && rs.Cloud.Cluster == s.Cluster && s.Cluster != rs.OnPremiseCluster && rs.Cloud.TemplateRef == s.TemplateRef && s.MaxWorkers <= rs.Cloud.MaxWorkers && s.Deadline == rs.Cloud.Deadline
	}
	cleanupOnly := false
	if !authorized() {
		cleanupRequested := expired || deleting
		for _, a := range s.Allocations {
			cleanupRequested = cleanupRequested || a.State == "Retire"
		}
		approved, ok := status["approvedRunSpec"].(map[string]interface{})
		if !cleanupRequested || !ok || status["approvedRunUID"] != s.RunRef.UID {
			return report("OwnerConflict", "capacity is not authorized by the current UID-bound Run contract")
		}
		// A changed parent cannot revoke cleanup of already authorized resources.
		// This snapshot authorizes retirement only, never fresh provisioning.
		run.Object["spec"] = approved
		if !authorized() {
			return report("OwnerConflict", "persisted cleanup contract does not match this capacity")
		}
		cleanupOnly = true
	}
	if !controllerutil.ContainsFinalizer(cap, Finalizer) {
		if deleting {
			return report("CleanupBlocked", "capacity deleted without lifecycle finalizer")
		}
		base := cap.DeepCopy()
		controllerutil.AddFinalizer(cap, Finalizer)
		if err = r.Patch(ctx, cap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		status["approvedRunUID"], status["approvedRunSpec"] = string(run.GetUID()), encode(rs)
		return report("BindingOwner", "allocation authorization persisted before provisioning")
	}
	seen := map[string]bool{}
	for _, a := range s.Allocations {
		if a.Name == "" || len(a.Name) > 63 || seen[a.Name] || (a.Market != "spot" && a.Market != "on-demand") || (a.State != "Prepare" && a.State != "Retain" && a.State != "Retire" && a.State != "Fence") {
			return report("Invalid", "unique allocation names, explicit markets and valid lifecycle states required")
		}
		seen[a.Name] = true
	}
	for name := range receipts {
		if !seen[name] {
			return report("InventoryConflict", "allocated entries cannot be removed; retain their retirement receipts")
		}
	}
	inventory, err := listItems(ctx, r.Client, "ml.dcn.ssu.ac.kr/v1alpha1", "NodeProvision", req.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range inventory {
		np := &inventory[i]
		if owned(np, cap) || np.GetLabels()[CapacityUIDLabel] == string(cap.GetUID()) {
			if !seen[np.GetName()] {
				return report("InventoryConflict", "unlisted owned allocation requires reconciliation before new provisioning")
			}
			if e, ok := receipts[np.GetName()].(map[string]interface{}); ok && e["phase"] == "Retired" {
				return report("InventoryConflict", "retired allocation still has a provisioning resource")
			}
		}
	}
	active := int64(0)
	for _, a := range s.Allocations {
		entry, _ := receipts[a.Name].(map[string]interface{})
		if entry == nil || entry["phase"] != "Retired" {
			active++
		}
	}
	if active > s.MaxWorkers {
		return report("LimitExceeded", "preparing, retained and unconfirmed retiring workers exceed the approved limit")
	}
	reader, err := r.Members(s.Cluster)
	if err != nil {
		return report("MemberUnavailable", err.Error())
	}
	raw, _ := status["templateJSON"].(string)
	if raw == "" {
		template := &corev1.ConfigMap{}
		if err = r.Get(ctx, key(req.Namespace, s.TemplateRef), template); err != nil {
			return report("TemplateUnavailable", err.Error())
		}
		if template.Immutable == nil || !*template.Immutable || template.UID == "" {
			return report("InvalidTemplate", "provisioning template must be an immutable ConfigMap")
		}
		raw = template.Data["nodeProvisionSpec.json"]
		sum := sha256.Sum256([]byte(raw))
		status["templateUID"], status["templateSHA256"], status["templateJSON"] = string(template.UID), hex.EncodeToString(sum[:]), raw
		if evidence := cap.GetAnnotations()[initialPolicyAnnotation]; evidence != "" {
			var receipt map[string]interface{}
			if json.Unmarshal([]byte(evidence), &receipt) != nil || receipt["templateUID"] != string(template.UID) || receipt["templateSHA256"] != hex.EncodeToString(sum[:]) {
				delete(status, "templateUID")
				delete(status, "templateSHA256")
				delete(status, "templateJSON")
				return report("TemplateConflict", "initial policy was evaluated against a different template")
			}
		}
		return report("BindingTemplate", "template identity persisted before allocation")
	}
	var spec map[string]interface{}
	if json.Unmarshal([]byte(raw), &spec) != nil {
		return report("InvalidTemplate", "invalid provisioning template JSON")
	}
	if spec["provider"] != "AWS" || spec["networkMode"] != "VPC" || spec["hardwareType"] != "gpu" || spec["instanceId"] != nil || spec["fence"] != nil {
		return report("InvalidTemplate", "only new AWS VPC GPU allocations with provider fencing are supported")
	}
	// Decode through Kubernetes JSON conversion so integer values remain int64.
	normalized := object("ml.dcn.ssu.ac.kr/v1alpha1", "NodeProvision")
	if err = json.Unmarshal([]byte(`{"apiVersion":"ml.dcn.ssu.ac.kr/v1alpha1","kind":"NodeProvision","spec":`+raw+`}`), normalized); err != nil {
		return report("InvalidTemplate", err.Error())
	}
	ready := int64(0)
	retired := int64(0)
	for _, a := range s.Allocations {
		entry, _ := receipts[a.Name].(map[string]interface{})
		if entry == nil {
			entry = map[string]interface{}{"market": a.Market, "phase": "Pending"}
			receipts[a.Name] = entry
		}
		if entry["market"] != a.Market {
			return report("IdentityConflict", "market cannot change within an allocation; create a replacement allocation")
		}
		retire := a.State == "Retire" || expired || deleting
		if cleanupOnly && !retire {
			// A different allocation's Retire request must not authorize replacing
			// or terminating this retained allocation after a parent contract edit.
			continue
		}
		if entry["phase"] == "Retired" {
			if !retire {
				return report("IdentityConflict", "retired allocation cannot be recreated")
			}
			retired++
			continue
		}
		np := p.NewObject("NodeProvision")
		np.SetNamespace(req.Namespace)
		np.SetName(a.Name)
		err = r.Get(ctx, key(req.Namespace, a.Name), np)
		if apierrors.IsNotFound(err) {
			if entry["originUID"] != nil {
				member := p.NewObject("NodeProvision")
				memberErr := reader.Get(ctx, key(req.Namespace, a.Name), member)
				if retire && (entry["fencedAt"] != nil || entry["neverDispatched"] == true) && apierrors.IsNotFound(memberErr) {
					entry["phase"], entry["retiredAt"] = "Retired", stamp(r.now())
					retired++
					continue
				}
				return report("CleanupUnconfirmed", "recorded allocation disappeared without complete provider and member cleanup evidence")
			}
			if retire {
				entry["phase"], entry["retiredAt"] = "Retired", stamp(r.now())
				retired++
				continue
			}
			if rs.Suspend {
				return report("Suspended", "new capacity creation suspended")
			}
			collision := p.NewObject("NodeProvision")
			if e := reader.Get(ctx, key(req.Namespace, a.Name), collision); !apierrors.IsNotFound(e) {
				return report("OwnershipConflict", "member allocation name is occupied or cannot be checked")
			}
			// Existing cleanup uses the pinned snapshot even if the template was
			// deleted. New allocation additionally requires the live reviewed UID.
			liveTemplate := &corev1.ConfigMap{}
			if e := r.Get(ctx, key(req.Namespace, s.TemplateRef), liveTemplate); e != nil {
				return report("TemplateUnavailable", e.Error())
			}
			liveSum := sha256.Sum256([]byte(liveTemplate.Data["nodeProvisionSpec.json"]))
			if status["templateUID"] != string(liveTemplate.UID) || status["templateSHA256"] != hex.EncodeToString(liveSum[:]) {
				return report("TemplateConflict", "reviewed provisioning template was replaced")
			}
			np.Object["spec"] = normalized.DeepCopy().Object["spec"]
			market := "OnDemand"
			if a.Market == "spot" {
				market = "Spot"
			}
			_ = unstructured.SetNestedField(np.Object, market, "spec", "marketType")
			_ = unstructured.SetNestedField(np.Object, a.Name, "spec", "hostname")
			np.SetLabels(map[string]string{RunUIDLabel: string(run.GetUID()), CapacityUIDLabel: string(cap.GetUID())})
			setOwner(np, cap)
			if err = r.Create(ctx, np); err != nil {
				return ctrl.Result{}, err
			}
			entry["originUID"], entry["requestedAt"], entry["phase"] = string(np.GetUID()), stamp(r.now()), "Created"
			return report("Preparing", "allocation UID recorded before propagation")
		} else if err != nil {
			return ctrl.Result{}, err
		}
		if !owned(np, cap) || np.GetLabels()[CapacityUIDLabel] != string(cap.GetUID()) || np.GetLabels()[RunUIDLabel] != string(run.GetUID()) {
			return report("OwnershipConflict", "refusing to adopt a foreign NodeProvision")
		}
		wantedMarket := "OnDemand"
		if a.Market == "spot" {
			wantedMarket = "Spot"
		}
		if str(np, "spec", "marketType") != wantedMarket || str(np, "spec", "provider") != "AWS" || str(np, "spec", "networkMode") != "VPC" {
			return report("IdentityConflict", "allocation provisioning contract changed")
		}
		if previous, ok := entry["originUID"].(string); ok && previous != string(np.GetUID()) {
			return report("IdentityConflict", "NodeProvision was recreated")
		}
		if entry["originUID"] == nil {
			entry["originUID"] = string(np.GetUID())
			return report("Preparing", "recovered allocation UID before propagation")
		}
		if rs.Suspend && !retire && a.State != "Fence" {
			entry["phase"] = "Retained"
			continue
		}
		if entry["propagationRequestedAt"] == nil {
			if retire {
				// The durable dispatch intent is always written before creating a
				// policy. A cancellation before that point must not launch a VM.
				member := p.NewObject("NodeProvision")
				if e := reader.Get(ctx, key(req.Namespace, a.Name), member); !apierrors.IsNotFound(e) {
					return report("CleanupUnconfirmed", "unexpected member allocation before dispatch intent")
				}
				if entry["neverDispatched"] != true {
					entry["neverDispatched"] = true
					return report("Retiring", "cancellation recorded before any provisioning dispatch")
				}
				uid := np.GetUID()
				if err = r.Delete(ctx, np, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
					return ctrl.Result{}, err
				}
				continue
			}
			entry["propagationRequestedAt"] = stamp(r.now())
			return report("Preparing", "dispatch intent persisted before member provisioning")
		}
		labels := np.GetLabels()
		if labels[originProvisionUID] != string(np.GetUID()) {
			base := np.DeepCopy()
			labels[originProvisionUID] = string(np.GetUID())
			np.SetLabels(labels)
			if err = r.Patch(ctx, np, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
		collision := p.NewObject("NodeProvision")
		if e := reader.Get(ctx, key(req.Namespace, a.Name), collision); e == nil {
			if collision.GetLabels()[originProvisionUID] != string(np.GetUID()) || collision.GetLabels()[CapacityUIDLabel] != string(cap.GetUID()) {
				return report("OwnershipConflict", "foreign member allocation blocks dispatch")
			}
		} else if !apierrors.IsNotFound(e) {
			return report("MemberUnavailable", e.Error())
		}
		if err = ensurePlacement(ctx, r.Client, cap, np, []string{s.Cluster}); err != nil {
			return ctrl.Result{}, err
		}
		member := p.NewObject("NodeProvision")
		if err = reader.Get(ctx, key(req.Namespace, a.Name), member); apierrors.IsNotFound(err) {
			entry["phase"] = "Propagating"
			continue
		} else if err != nil {
			return report("MemberUnavailable", err.Error())
		}
		if member.GetLabels()[originProvisionUID] != string(np.GetUID()) || member.GetLabels()[CapacityUIDLabel] != string(cap.GetUID()) {
			return report("IdentityConflict", "member NodeProvision origin identity mismatch")
		}
		if previous, ok := entry["memberUID"].(string); ok && previous != string(member.GetUID()) {
			return report("IdentityConflict", "member NodeProvision was recreated")
		}
		entry["memberUID"] = string(member.GetUID())
		if spot, ok, _ := unstructured.NestedMap(member.Object, "status", "spot"); ok {
			entry["spot"] = spot
		}
		entry["memberGeneration"] = member.GetGeneration()
		instance := str(member, "status", "instanceId")
		if previous, ok := entry["instanceID"].(string); ok && instance != "" && previous != instance {
			return report("IdentityConflict", "allocation instance ID changed")
		}
		if instance != "" {
			entry["instanceID"] = instance
		}
		if retire || a.State == "Fence" {
			if instance == "" {
				entry["phase"] = "RetirementPending"
				continue
			}
			operation, fenceErr := authorizedFenceOperation(ctx, r.Client, cap, run, a, entry, instance)
			if fenceErr != nil {
				return report("FenceConflict", fenceErr.Error())
			}
			request := map[string]interface{}{"operationUID": operation, "instanceID": instance}
			old, found, _ := unstructured.NestedMap(np.Object, "spec", "fence")
			if !found {
				base := np.DeepCopy()
				_ = unstructured.SetNestedMap(np.Object, request, "spec", "fence")
				if err = r.Patch(ctx, np, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					return ctrl.Result{}, err
				}
				entry["phase"] = "Fencing"
				continue
			}
			if old["operationUID"] != operation || old["instanceID"] != instance {
				return report("FenceConflict", "foreign fence intent")
			}
			if str(member, "status", "fence", "phase") != "Fenced" || str(member, "status", "fence", "operationUID") != operation || str(member, "status", "fence", "instanceID") != instance || integer(member, "status", "fence", "observedGeneration") != member.GetGeneration() {
				entry["phase"] = "Fencing"
				continue
			}
			if entry["fencedAt"] == nil {
				entry["fencedAt"], entry["phase"], entry["fenceOperationUID"] = str(member, "status", "fence", "observedAt"), "Fenced", operation
				if entry["fencedAt"] == "" {
					return report("FenceConflict", "provider fence timestamp missing")
				}
				return report("Retiring", "provider-confirmed termination receipt persisted")
			}
			if !retire {
				entry["phase"] = "Fenced"
				continue
			}
			if np.GetDeletionTimestamp().IsZero() {
				uid := np.GetUID()
				rv := np.GetResourceVersion()
				if err = r.Delete(ctx, np, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil {
					return ctrl.Result{}, err
				}
			}
			entry["phase"] = "Deleting"
			continue
		}
		if rs.Suspend {
			entry["phase"] = "Retained"
			continue
		}
		if str(member, "status", "phase") != "Ready" || instance == "" || !member.GetDeletionTimestamp().IsZero() {
			entry["phase"] = "Preparing"
			continue
		}
		node := &corev1.Node{}
		if err = reader.Get(ctx, client.ObjectKey{Name: str(member, "status", "nodeName")}, node); err != nil {
			entry["phase"] = "WaitingNode"
			continue
		}
		if node.Labels["ml.dcn.ssu.ac.kr/node-provision-uid"] != string(member.GetUID()) || !strings.HasSuffix(node.Spec.ProviderID, "/"+instance) {
			return report("IdentityConflict", "GPU Node ownership/provider ID mismatch")
		}
		healthy := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				healthy = true
			}
		}
		gpu := node.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
		if !healthy || node.Spec.Unschedulable || !node.DeletionTimestamp.IsZero() || gpu.Value() < 1 {
			entry["phase"] = "WaitingGPU"
			continue
		}
		entry["phase"], entry["nodeName"], entry["nodeUID"] = "Ready", node.Name, string(node.UID)
		ready++
	}
	status["readyWorkers"] = ready
	if retired == int64(len(s.Allocations)) {
		if deleting {
			base := cap.DeepCopy()
			controllerutil.RemoveFinalizer(cap, Finalizer)
			return ctrl.Result{}, r.Patch(ctx, cap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}
		return report("Retired", "all allocation retirement receipts preserved")
	}
	if ready == active {
		return report("Ready", "all retained GPU allocations are UID-bound and Ready; workload readiness is checked separately")
	}
	retirementRequested := expired || deleting
	for _, a := range s.Allocations {
		retirementRequested = retirementRequested || a.State == "Retire"
	}
	if retirementRequested {
		return report("Retiring", "deadline or deletion requests provider-confirmed cleanup")
	}
	return report("Preparing", "waiting for allocation readiness or retirement")
}

func SetupCapacity(mgr ctrl.Manager) error {
	r := &CapacityReconciler{Client: mgr.GetClient(), Members: configuredMembers()}
	return ctrl.NewControllerManagedBy(mgr).For(resource.Object("TrainingCapacity")).WithEventFilter(predicate.GenerationChangedPredicate{}).Complete(r)
}
