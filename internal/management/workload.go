package management

import (
	"context"
	"fmt"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateLiveWorkloadUID(ctx context.Context, c client.Client, input trainingpolicy.PolicyInput) error {
	if input.WorkloadRef.APIVersion != "apps/v1" || input.WorkloadRef.Kind != "StatefulSet" {
		return fmt.Errorf("spec.workloadRef must identify apps/v1 StatefulSet")
	}
	if input.WorkloadRef.Name == "" || input.WorkloadRef.UID == "" {
		return fmt.Errorf("spec.workloadRef name and uid are required")
	}
	workload := trainingpolicy.NewObject("StatefulSet")
	err := c.Get(ctx, types.NamespacedName{Namespace: input.Namespace, Name: input.WorkloadRef.Name}, workload)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("live StatefulSet %s/%s not found", input.Namespace, input.WorkloadRef.Name)
	}
	if err != nil {
		return fmt.Errorf("get live StatefulSet %s/%s: %w", input.Namespace, input.WorkloadRef.Name, err)
	}
	if workload.GetUID() != input.WorkloadRef.UID {
		return fmt.Errorf("live StatefulSet uid %q does not match policy workloadRef uid %q", workload.GetUID(), input.WorkloadRef.UID)
	}
	return nil
}
