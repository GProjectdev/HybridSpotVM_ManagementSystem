package resource

import (
	"context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

const Group = "training.dcnlab.com"

func Object(kind string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: Group, Version: "v1alpha1", Kind: kind})
	return o
}
func List(kind string) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(schema.GroupVersionKind{Group: Group, Version: "v1alpha1", Kind: kind + "List"})
	return l
}
func String(o *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(o.Object, fields...)
	return v
}
func Int(o *unstructured.Unstructured, fields ...string) int64 {
	v, _, _ := unstructured.NestedInt64(o.Object, fields...)
	return v
}
func Timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func SetStatus(ctx context.Context, c client.Client, o *unstructured.Unstructured, status map[string]interface{}) error {
	base := o.DeepCopy()
	o.Object["status"] = status
	return c.Status().Patch(ctx, o, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
