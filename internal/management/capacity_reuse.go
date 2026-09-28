package management

import (
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func reusableCapacityReplacement(candidate, old *unstructured.Unstructured, input p.PolicyInput) bool {
	if candidate == nil || candidate.GetUID() == "" || !candidate.GetDeletionTimestamp().IsZero() {
		return false
	}
	labels, annotations := candidate.GetLabels(), candidate.GetAnnotations()
	return candidate.GetNamespace() == input.Namespace &&
		candidate.GetName() == replacementNodeProvisionName(old.GetName(), string(old.GetUID())) &&
		labels[p.LabelPolicyUID] == string(input.PolicyUID) &&
		labels[p.LabelPolicy] == input.PolicyName &&
		labels[p.LabelManagedBy] == "hybridspotvm-system" &&
		labels[p.LabelRole] == "replacement" &&
		annotations["training.dcnlab.com/replaces-nodeprovision"] == old.GetName() &&
		annotations["training.dcnlab.com/replaces-nodeprovision-uid"] == string(old.GetUID()) &&
		annotations["training.dcnlab.com/recovery-operation"] == replacementOperationName(old.GetName(), string(old.GetUID())) &&
		stringField(candidate.Object, "spec", "marketType") == "OnDemand"
}
