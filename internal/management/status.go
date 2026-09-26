package management

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func patchStatusSubtree(ctx context.Context, c client.Client, obj *unstructured.Unstructured, key string, value map[string]interface{}) error {
	patch := statusSubtreePatch(obj, key, value)
	raw, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal status patch: %w", err)
	}
	return c.Status().Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
}

func statusSubtreePatch(obj *unstructured.Unstructured, key string, value map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"metadata": map[string]interface{}{
			"uid":             string(obj.GetUID()),
			"resourceVersion": obj.GetResourceVersion(),
		},
		"status": map[string]interface{}{
			key: value,
		},
	}
}
