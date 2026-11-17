package management

import (
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// configuredAWSCluster keeps the legacy default while binding actuators to the
// AWS member explicitly selected by the UID-checked NodeRequest.
func configuredAWSCluster(policy *unstructured.Unstructured) string {
	cluster := stringField(policy.Object, "spec", "capacity", "aws", "karmadaCluster")
	if cluster == "" {
		return p.DefaultAWSCluster
	}
	return cluster
}
