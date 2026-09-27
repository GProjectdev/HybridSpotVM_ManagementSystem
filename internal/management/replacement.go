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
	uid := strings.ToLower(oldNodeProvisionUID)
	uid = strings.ReplaceAll(uid, "_", "-")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	if uid == "" {
		return fmt.Sprintf("%s-replace", oldNodeProvisionName)
	}
	return fmt.Sprintf("%s-%s-replace", oldNodeProvisionName, uid)
}

func replacementNodeProvisionName(oldNodeProvisionName string) string {
	return fmt.Sprintf("%s-replacement", oldNodeProvisionName)
}
