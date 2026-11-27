package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const initialPolicyAnnotation = "training.dcnlab.com/initial-policy-evidence"

func resourcePolicyInput(run, template *unstructured.Unstructured, s RunSpec) (p.PolicyInput, error) {
	if err := s.ResourcePolicy.validateMethod(s.WorldSize); err != nil {
		return p.PolicyInput{}, err
	}
	alpha := p.DefaultAlpha
	if s.ResourcePolicy.Alpha != nil {
		alpha = *s.ResourcePolicy.Alpha
	}
	horizon := s.ResourcePolicy.ForecastSeconds
	if horizon == 0 {
		horizon = p.DefaultForecastHorizonSeconds
	}
	if math.IsNaN(alpha) || math.IsInf(alpha, 0) || alpha <= 0 || alpha > 1 || horizon < 1 || horizon > 86400 || s.ResourcePolicy.MinOnDemand < 0 || s.ResourcePolicy.MinOnDemand > s.WorldSize {
		return p.PolicyInput{}, fmt.Errorf("invalid alpha, forecast horizon or minimum On-Demand workers")
	}
	return p.PolicyInput{Namespace: run.GetNamespace(), PolicyName: run.GetName(), PolicyUID: run.GetUID(), Generation: run.GetGeneration(),
		TargetWorkers: s.WorldSize, MinOnDemand: s.ResourcePolicy.MinOnDemand, Alpha: alpha, ForecastSeconds: horizon, VMPolicyMethod: s.ResourcePolicy.method(), GroupRisk: s.ResourcePolicy.GroupRisk,
		Capacity:   p.CapacityDefaults{AWSCluster: s.Cloud.Cluster, Region: str(template, "spec", "region"), AvailabilityZone: str(template, "spec", "awsConfig", "availabilityZone"), InstanceType: str(template, "spec", "instanceType")},
		Checkpoint: p.CheckpointPolicy{Resume: true, MinIntervalSeconds: 1, MaxIntervalSeconds: min(s.CheckpointSeconds, MaximumCheckpointSeconds)}}, nil
}

// initialCloudDecision uses the existing policy calculation. Initial allocation
// has no measured eviction loss yet; the decision explicitly retains Bootstrap.
func initialCloudDecision(ctx context.Context, c client.Reader, run *unstructured.Unstructured, s RunSpec, now time.Time) (p.Decision, map[string]interface{}, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, key(run.GetNamespace(), s.Cloud.TemplateRef), cm); err != nil {
		return p.Decision{}, nil, err
	}
	if cm.Immutable == nil || !*cm.Immutable || cm.UID == "" {
		return p.Decision{}, nil, fmt.Errorf("immutable identified capacity template required")
	}
	template := object("ml.dcn.ssu.ac.kr/v1alpha1", "NodeProvision")
	if err := json.Unmarshal([]byte(`{"apiVersion":"ml.dcn.ssu.ac.kr/v1alpha1","kind":"NodeProvision","spec":`+cm.Data["nodeProvisionSpec.json"]+`}`), template); err != nil {
		return p.Decision{}, nil, err
	}
	if str(template, "spec", "provider") != "AWS" || str(template, "spec", "networkMode") != "VPC" || str(template, "spec", "hardwareType") != "gpu" {
		return p.Decision{}, nil, fmt.Errorf("new AWS VPC GPU template required")
	}
	input, err := resourcePolicyInput(run, template, s)
	if err != nil {
		return p.Decision{}, nil, err
	}
	riskObject := p.NewObject("SpotRiskProfile")
	if err := c.Get(ctx, key(run.GetNamespace(), s.Cloud.RiskProfileRef), riskObject); err != nil {
		return p.Decision{}, nil, err
	}
	risk := p.ReadRiskStatus(riskObject)
	at, err := time.Parse(time.RFC3339Nano, risk.ObservedAt)
	if err != nil || at.After(now) || !p.RiskFreshForPolicy(input, risk, now) || riskObject.GetUID() == "" || !riskObject.GetDeletionTimestamp().IsZero() {
		return p.Decision{}, nil, fmt.Errorf("fresh current-generation risk and price evidence required")
	}
	if risk.Provider != "AWS" || risk.Region == "" || risk.Region != input.Capacity.Region || risk.AvailabilityZone == "" || risk.AvailabilityZone != input.Capacity.AvailabilityZone || risk.InstanceType == "" || risk.InstanceType != input.Capacity.InstanceType {
		return p.Decision{}, nil, fmt.Errorf("risk market does not match the reviewed capacity template")
	}
	if risk.SpotPricePerHour <= 0 || risk.OnDemandPricePerHour <= 0 || math.IsNaN(risk.SpotPricePerHour) || math.IsNaN(risk.OnDemandPricePerHour) || math.IsInf(risk.SpotPricePerHour, 0) || math.IsInf(risk.OnDemandPricePerHour, 0) {
		return p.Decision{}, nil, fmt.Errorf("positive finite prices for both markets required")
	}
	d := p.DecideAt(input, p.RuntimeSnapshot{}, risk, now)
	if d.ProvisioningBlocked || d.SpotWorkers+d.OnDemandWorkers != s.WorldSize {
		return d, nil, fmt.Errorf("policy blocks allocation: %s", d.Reason)
	}
	receipt := map[string]interface{}{"observedAt": stamp(now), "riskUID": string(riskObject.GetUID()), "riskGeneration": riskObject.GetGeneration(), "riskResourceVersion": riskObject.GetResourceVersion(), "riskSource": risk.Source, "riskObservedAt": risk.ObservedAt, "riskValidUntil": risk.ValidUntil, "lambdaPerHour": risk.LambdaPerHour, "spotPricePerHour": risk.SpotPricePerHour, "onDemandPricePerHour": risk.OnDemandPricePerHour, "templateUID": string(cm.UID), "decision": encode(d)}
	if source, found, err := unstructured.NestedFieldCopy(riskObject.Object, "status", "source"); err == nil && found {
		receipt["riskSource"] = source
	}
	sum := sha256.Sum256([]byte(cm.Data["nodeProvisionSpec.json"]))
	receipt["templateSHA256"] = hex.EncodeToString(sum[:])
	return d, receipt, nil
}

func initialCapacityName(run *unstructured.Unstructured) string {
	return "run-" + string(run.GetUID()) + "-initial"
}

func (r *RunReconciler) initialCapacity(ctx context.Context, run *unstructured.Unstructured, s RunSpec, status map[string]interface{}, create bool) (*unstructured.Unstructured, error) {
	cap := named("TrainingCapacity", run.GetNamespace(), initialCapacityName(run))
	err := r.Get(ctx, client.ObjectKeyFromObject(cap), cap)
	if apierrors.IsNotFound(err) {
		if status["initialCapacityUID"] != nil {
			return nil, fmt.Errorf("recorded capacity disappeared; automatic recreation prohibited")
		}
		if !create {
			return nil, nil
		}
		deadline, err := time.Parse(time.RFC3339, s.Cloud.Deadline)
		if err != nil || !r.now().Before(deadline) {
			return nil, fmt.Errorf("capacity deadline expired")
		}
		d, receipt, err := initialCloudDecision(ctx, r.Client, run, s, r.now())
		if err != nil {
			return nil, err
		}
		allocations := make([]Allocation, 0, s.WorldSize)
		for i := int64(0); i < s.WorldSize; i++ {
			market := "spot"
			if i < d.OnDemandWorkers {
				market = "on-demand"
			}
			allocations = append(allocations, Allocation{Name: fmt.Sprintf("run-%s-%d", run.GetUID(), i), Market: market, State: "Prepare"})
		}
		cap.Object["spec"] = encode(CapacitySpec{RunRef: ObjectRef{Name: run.GetName(), UID: string(run.GetUID())}, Cluster: s.Cloud.Cluster, TemplateRef: s.Cloud.TemplateRef, MaxWorkers: s.Cloud.MaxWorkers, Deadline: s.Cloud.Deadline, Allocations: allocations})
		b, _ := json.Marshal(receipt)
		cap.SetAnnotations(map[string]string{initialPolicyAnnotation: string(b)})
		setOwner(cap, run)
		if err = r.Create(ctx, cap); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if !owned(cap, run) || str(cap, "spec", "runRef", "uid") != string(run.GetUID()) || str(cap, "spec", "cluster") != s.Cloud.Cluster || str(cap, "spec", "templateRef") != s.Cloud.TemplateRef || !cap.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("capacity ownership/contract mismatch")
	}
	if previous, ok := status["initialCapacityUID"].(string); ok && previous != string(cap.GetUID()) {
		return nil, fmt.Errorf("capacity UID changed")
	}
	if cap.GetUID() != "" {
		status["initialCapacityUID"] = string(cap.GetUID())
	}
	if raw := cap.GetAnnotations()[initialPolicyAnnotation]; raw != "" {
		var receipt map[string]interface{}
		if json.Unmarshal([]byte(raw), &receipt) != nil {
			return nil, fmt.Errorf("invalid initial policy evidence")
		}
		// Decode using Kubernetes JSON to preserve integer values for API status.
		carrier := &unstructured.Unstructured{}
		if err := json.Unmarshal([]byte(`{"apiVersion":"v1","kind":"ConfigMap","receipt":`+raw+`}`), carrier); err != nil {
			return nil, err
		}
		status["initialCloudPolicy"] = carrier.Object["receipt"]
	}
	return cap, nil
}

func capacityReady(cap *unstructured.Unstructured, world int64, now time.Time) bool {
	if cap == nil || cap.GetUID() == "" || !cap.GetDeletionTimestamp().IsZero() || str(cap, "status", "phase") != "Ready" || integer(cap, "status", "observedGeneration") != cap.GetGeneration() || integer(cap, "status", "readyWorkers") != world {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, str(cap, "status", "observedAt"))
	if err != nil || at.After(now) || now.Sub(at) > 30*time.Second {
		return false
	}
	var s CapacitySpec
	if decodeSpec(cap, &s) != nil || int64(len(s.Allocations)) != world {
		return false
	}
	deadline, err := time.Parse(time.RFC3339, s.Deadline)
	if err != nil || !now.Before(deadline) {
		return false
	}
	for _, a := range s.Allocations {
		if a.State == "Retire" || str(cap, "status", "allocations", a.Name, "phase") != "Ready" || str(cap, "status", "allocations", a.Name, "nodeUID") == "" || str(cap, "status", "allocations", a.Name, "instanceID") == "" {
			return false
		}
	}
	return true
}

func retireUnusedCapacity(ctx context.Context, c client.Client, cap *unstructured.Unstructured) error {
	if cap == nil {
		return nil
	}
	allocations, _, err := unstructured.NestedSlice(cap.Object, "spec", "allocations")
	if err != nil {
		return err
	}
	base := cap.DeepCopy()
	changed := false
	for _, raw := range allocations {
		a, ok := raw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid allocation")
		}
		if a["state"] != "Retire" {
			a["state"] = "Retire"
			changed = true
		}
	}
	if !changed {
		return nil
	}
	_ = unstructured.SetNestedSlice(cap.Object, allocations, "spec", "allocations")
	return c.Patch(ctx, cap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
