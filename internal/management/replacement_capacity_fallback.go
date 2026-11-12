package management

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (r *ReplacementReconciler) prepareFallbackCapacity(ctx context.Context, op *unstructured.Unstructured, spec replacementSpec, old *unstructured.Unstructured) error {
	fallback, recorded, _ := unstructured.NestedMap(op.Object, "status", "partialFallback")
	if !recorded || stringField(fallback, "phase") != partialFallbackHandoffRecorded {
		return fmt.Errorf("waiting for coordinator fallback intent")
	}
	if err := verifyFallbackOldNode(old, spec, fallback); err != nil {
		return err
	}
	if err := r.validateRecordedPartialFallback(ctx, op.GetNamespace(), spec, fallback); err != nil {
		return err
	}
	_, input, err := r.fallbackPolicyInput(ctx, op.GetNamespace(), spec)
	if err != nil {
		return err
	}
	if err := r.revalidateFreshPartialFallbackProof(ctx, op.GetNamespace(), spec, input, fallback); err != nil {
		return err
	}
	return r.prepareReplacementForGroupFallback(ctx, op.GetNamespace(), spec, old, input)
}
