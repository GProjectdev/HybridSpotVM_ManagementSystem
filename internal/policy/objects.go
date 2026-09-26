package policy

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const (
	LabelManagedBy = "training.dcnlab.com/managed-by"
	LabelPolicy    = "training.dcnlab.com/policy"
	LabelPolicyUID = "training.dcnlab.com/policy-uid"
	LabelRole      = "training.dcnlab.com/role"
)

func NewObject(kind string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	switch kind {
	case "TrainingPolicy":
		obj.SetGroupVersionKind(TrainingPolicyGVK)
	case "SpotRiskProfile":
		obj.SetGroupVersionKind(SpotRiskProfileGVK)
	case "TrainingRuntime":
		obj.SetGroupVersionKind(TrainingRuntimeGVK)
	case "SpotRecovery":
		obj.SetGroupVersionKind(SpotRecoveryGVK)
	case "StatefulSet":
		obj.SetGroupVersionKind(StatefulSetGVK)
	case "NodeProvision":
		obj.SetGroupVersionKind(NodeProvisionGVK)
	case "FluidCRMigration":
		obj.SetGroupVersionKind(FluidMigrationGVK)
	case "PropagationPolicy":
		obj.SetGroupVersionKind(PropagationPolicyGVK)
	}
	return obj
}

func NewList(kind string) *unstructured.UnstructuredList {
	obj := NewObject(kind)
	list := &unstructured.UnstructuredList{}
	gvk := obj.GroupVersionKind()
	gvk.Kind += "List"
	list.SetGroupVersionKind(gvk)
	return list
}

func NewNodeProvision(input PolicyInput, ordinal int64, marketType string) *unstructured.Unstructured {
	name := fmt.Sprintf("%s-worker-%02d", input.PolicyName, ordinal)
	obj := NewObject("NodeProvision")
	obj.SetNamespace(input.Namespace)
	obj.SetName(name)
	obj.SetLabels(ownerLabels(input, "worker"))
	hardwareType := input.Capacity.HardwareType
	if hardwareType == "" {
		hardwareType = "cpu"
	}
	nodeLabel := input.Capacity.NodeLabel
	if nodeLabel == "" {
		nodeLabel = hardwareType
	}
	spec := map[string]interface{}{
		"provider":    "AWS",
		"marketType":  marketType,
		"role":        "worker",
		"nodeLabel":   nodeLabel,
		"networkMode": "VPC",
	}
	setIfString(spec, "region", input.Capacity.Region)
	setIfString(spec, "instanceType", input.Capacity.InstanceType)
	setIfString(spec, "hostname", name)
	setIfString(spec, "hardwareType", hardwareType)
	aws := map[string]interface{}{}
	setIfString(aws, "ami", input.Capacity.AMI)
	setIfString(aws, "availabilityZone", input.Capacity.AvailabilityZone)
	setIfString(aws, "subnetId", input.Capacity.SubnetID)
	setIfString(aws, "vpcId", input.Capacity.VPCID)
	setIfString(aws, "iamInstanceProfile", input.Capacity.IAMInstanceProfile)
	if input.Capacity.RootVolumeSizeGB > 0 {
		aws["rootVolumeSizeGB"] = input.Capacity.RootVolumeSizeGB
	}
	if len(input.Capacity.SecurityGroupIDs) > 0 {
		aws["securityGroupIds"] = input.Capacity.SecurityGroupIDs
	}
	if len(aws) > 0 {
		spec["awsConfig"] = aws
	}
	if input.Capacity.CredentialsName != "" {
		credentials := map[string]interface{}{"name": input.Capacity.CredentialsName}
		if input.Capacity.CredentialsNS != "" {
			credentials["namespace"] = input.Capacity.CredentialsNS
		}
		spec["credentialsRef"] = credentials
	}
	obj.Object["spec"] = spec
	return obj
}

func NewPropagationPolicyFor(input PolicyInput, obj *unstructured.Unstructured, clusterName string) *unstructured.Unstructured {
	pp := NewObject("PropagationPolicy")
	pp.SetNamespace(obj.GetNamespace())
	pp.SetName(obj.GetName() + "-placement")
	pp.SetLabels(ownerLabels(input, "placement"))
	pp.Object["spec"] = map[string]interface{}{
		"resourceSelectors": []interface{}{map[string]interface{}{
			"apiVersion": obj.GetAPIVersion(),
			"kind":       obj.GetKind(),
			"name":       obj.GetName(),
		}},
		"placement": map[string]interface{}{
			"clusterAffinity": map[string]interface{}{
				"clusterNames": []interface{}{clusterName},
			},
		},
	}
	return pp
}

func NewFluidCRMigration(input PolicyInput, runtime RuntimeSnapshot, startedAt time.Time, intervalSeconds int64) *unstructured.Unstructured {
	if intervalSeconds <= 0 {
		intervalSeconds = DefaultCheckpointSeconds
	}
	round := startedAt.Unix() / intervalSeconds
	checkpointID := fmt.Sprintf("%s-ckpt-%d", input.PolicyName, round)
	obj := NewObject("FluidCRMigration")
	obj.SetNamespace(input.Namespace)
	obj.SetName(checkpointID)
	obj.SetLabels(ownerLabels(input, "checkpoint"))
	obj.SetAnnotations(map[string]string{
		"training.dcnlab.com/started-at":    startedAt.UTC().Format(time.RFC3339),
		"training.dcnlab.com/checkpoint-id": checkpointID,
	})
	workloadRef := map[string]interface{}{
		"apiVersion": input.WorkloadRef.APIVersion,
		"kind":       input.WorkloadRef.Kind,
		"name":       input.WorkloadRef.Name,
	}
	if input.WorkloadRef.UID != "" {
		workloadRef["uid"] = string(input.WorkloadRef.UID)
	}
	spec := map[string]interface{}{
		"workloadRef": workloadRef,
		"resume":      input.Checkpoint.Resume,
		"ctrlPort":    runtime.Port,
	}
	if runtime.Container != "" {
		spec["container"] = runtime.Container
	}
	obj.Object["spec"] = spec
	return obj
}

func PolicyStatus(decision Decision, observedAt time.Time) map[string]interface{} {
	return map[string]interface{}{
		"observedAt":                            observedAt.UTC().Format(time.RFC3339),
		"desiredWorkers":                        decision.DesiredWorkers,
		"onDemandWorkers":                       decision.OnDemandWorkers,
		"spotWorkers":                           decision.SpotWorkers,
		"alpha":                                 decision.Alpha,
		"forecastHorizonSeconds":                decision.ForecastHorizonSeconds,
		"lambdaPerHour":                         decision.LambdaPerHour,
		"checkpointIntervalSeconds":             decision.CheckpointIntervalSeconds,
		"costEvaluated":                         decision.CostEvaluated,
		"intervalCostEvaluated":                 decision.IntervalCostEvaluated,
		"provisioningBlocked":                   decision.ProvisioningBlocked,
		"reason":                                decision.Reason,
		"replacementRequiresUIDBoundOperation":  true,
		"deleteRequiresExplicitRestoreEvidence": true,
	}
}

func CheckpointStatus(name string, intervalSeconds int64, observedAt time.Time, reason string) map[string]interface{} {
	return map[string]interface{}{
		"observedAt":                  observedAt.UTC().Format(time.RFC3339),
		"lastMigrationName":           name,
		"checkpointIntervalSeconds":   intervalSeconds,
		"reason":                      reason,
		"deduplication":               "restart,inflight,emergency,no-catchup",
		"sourcePropagationPolicyOnly": true,
	}
}

func OwnedByPolicy(obj *unstructured.Unstructured, uid types.UID) bool {
	return obj.GetLabels()[LabelPolicyUID] == string(uid)
}

func ownerLabels(input PolicyInput, role string) map[string]string {
	return map[string]string{
		LabelManagedBy: "hybridspotvm-system",
		LabelPolicy:    input.PolicyName,
		LabelPolicyUID: string(input.PolicyUID),
		LabelRole:      role,
	}
}

func setIfString(dst map[string]interface{}, key string, value string) {
	if value != "" {
		dst[key] = value
	}
}
