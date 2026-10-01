package management

import (
	"context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestCoordinatorDoesNotRecreatePartialAfterFallbackHandoff(t *testing.T) {
	op := newSpotReplacementObject()
	_ = unstructured.SetNestedMap(op.Object, map[string]interface{}{"phase": "HandoffRecorded"}, "status", "partialFallback")
	r := &CheckpointReconciler{}
	if err := r.coordinateReplacementCheckpoint(context.Background(), op); err == nil {
		t.Fatal("group handoff must fence partial creation before client side effects")
	}
}
