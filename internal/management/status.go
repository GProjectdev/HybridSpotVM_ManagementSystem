package management

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func patchStatusSubtree(ctx context.Context, c client.Client, obj *unstructured.Unstructured, key string, value map[string]interface{}) error {
	objectKey := client.ObjectKeyFromObject(obj)
	expectedUID := obj.GetUID()

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(obj.GroupVersionKind())
		if err := c.Get(ctx, objectKey, current); err != nil {
			return err
		}
		if expectedUID != "" && current.GetUID() != expectedUID {
			return fmt.Errorf("status target %s was recreated: uid %q, want %q", objectKey, current.GetUID(), expectedUID)
		}

		patch := statusSubtreePatch(current, key, value)
		raw, err := json.Marshal(patch)
		if err != nil {
			return fmt.Errorf("marshal status patch: %w", err)
		}
		return c.Status().Patch(ctx, current, client.RawPatch(types.MergePatchType, raw))
	})
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
