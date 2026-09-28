package management

import (
	"fmt"
	"strings"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var spotReplacementGVK = schema.GroupVersionKind{Group: trainingpolicy.Group, Version: trainingpolicy.Version, Kind: "SpotReplacement"}

func newSpotReplacementObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(spotReplacementGVK)
	return obj
}

func newSpotReplacementList() *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(spotReplacementGVK.GroupVersion().WithKind("SpotReplacementList"))
	return list
}

func replacementOperationName(oldNodeProvisionName, oldNodeProvisionUID string) string {
	return boundedReplacementName(oldNodeProvisionName, oldNodeProvisionUID, "replace")
}

func replacementNodeProvisionName(oldNodeProvisionName, oldNodeProvisionUID string) string {
	return boundedReplacementName(oldNodeProvisionName, oldNodeProvisionUID, "replacement")
}

func boundedReplacementName(oldNodeProvisionName, oldNodeProvisionUID, suffix string) string {
	uid := strings.ToLower(oldNodeProvisionUID)
	uid = strings.ReplaceAll(uid, "_", "-")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	fullSuffix := "-" + suffix
	if uid != "" {
		fullSuffix = "-" + uid + fullSuffix
	}
	prefix := dnsLabelPrefix(oldNodeProvisionName, 63-len(fullSuffix))
	if prefix == "" {
		prefix = "node"
	}
	return fmt.Sprintf("%s%s", prefix, fullSuffix)
}

func validReplacementMarket(market string) bool {
	return market == "Spot" || market == "OnDemand"
}
