package management

import (
	"fmt"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func reusableCapacityReplacement(candidate, old *unstructured.Unstructured, input p.PolicyInput, desiredMarket string) bool {
	if candidate == nil || candidate.GetUID() == "" || !candidate.GetDeletionTimestamp().IsZero() {
		return false
	}
	spec := replacementSpec{
		Operation:                    replacementOperationName(old.GetName(), string(old.GetUID())),
		PolicyName:                   input.PolicyName,
		PolicyUID:                    string(input.PolicyUID),
		OldNodeProvisionName:         old.GetName(),
		OldNodeProvisionUID:          string(old.GetUID()),
		ReplacementNodeProvisionName: replacementNodeProvisionName(old.GetName(), string(old.GetUID())),
		DesiredMarketType:            desiredMarket,
	}
	return replacementNodeProvisionIdentityError(candidate, input.Namespace, spec) == nil
}

func replacementNodeProvisionIdentityError(candidate *unstructured.Unstructured, ns string, spec replacementSpec) error {
	if candidate == nil {
		return fmt.Errorf("replacement NodeProvision is missing")
	}
	if candidate.GetUID() == "" || !candidate.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("existing replacement NodeProvision UID and non-deleting state are required")
	}
	labels, annotations := candidate.GetLabels(), candidate.GetAnnotations()
	if candidate.GetNamespace() == ns &&
		candidate.GetName() == spec.ReplacementNodeProvisionName &&
		labels[p.LabelPolicyUID] == spec.PolicyUID &&
		labels[p.LabelPolicy] == spec.PolicyName &&
		labels[p.LabelManagedBy] == "hybridspotvm-system" &&
		labels[p.LabelRole] == "replacement" &&
		annotations["training.dcnlab.com/replaces-nodeprovision"] == spec.OldNodeProvisionName &&
		annotations["training.dcnlab.com/replaces-nodeprovision-uid"] == spec.OldNodeProvisionUID &&
		annotations["training.dcnlab.com/recovery-operation"] == spec.Operation &&
		stringField(candidate.Object, "spec", "marketType") == spec.DesiredMarketType &&
		stringField(candidate.Object, "spec", "hostname") == spec.ReplacementNodeProvisionName {
		return nil
	}
	return fmt.Errorf("existing replacement NodeProvision identity mismatch")
}

func buildReplacementNodeProvision(ns string, oldNP *unstructured.Unstructured, spec replacementSpec) (*unstructured.Unstructured, error) {
	desired := p.NewObject("NodeProvision")
	desired.SetNamespace(ns)
	desired.SetName(spec.ReplacementNodeProvisionName)
	desired.SetLabels(map[string]string{
		p.LabelManagedBy: "hybridspotvm-system",
		p.LabelPolicy:    spec.PolicyName,
		p.LabelPolicyUID: spec.PolicyUID,
		p.LabelRole:      "replacement",
	})
	desired.SetAnnotations(map[string]string{
		"training.dcnlab.com/recovery-operation":         spec.Operation,
		"training.dcnlab.com/replaces-nodeprovision":     spec.OldNodeProvisionName,
		"training.dcnlab.com/replaces-nodeprovision-uid": spec.OldNodeProvisionUID,
	})
	sourceSpec, ok, _ := unstructured.NestedMap(oldNP.Object, "spec")
	if !ok {
		return nil, fmt.Errorf("old NodeProvision spec required")
	}
	newSpec := deepCopyMap(sourceSpec)
	delete(newSpec, "fence")
	newSpec["marketType"] = spec.DesiredMarketType
	newSpec["hostname"] = spec.ReplacementNodeProvisionName
	if awsConfig, ok := newSpec["awsConfig"].(map[string]interface{}); ok {
		delete(awsConfig, "keyPairName")
	}
	desired.Object["spec"] = newSpec
	return desired, nil
}
