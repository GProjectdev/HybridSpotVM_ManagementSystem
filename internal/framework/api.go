package framework

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const RunUIDLabel = "training.dcnlab.com/run-uid"
const CapacityUIDLabel = "training.dcnlab.com/capacity-uid"
const FrameworkManagedBy = "hybrid-training-framework"
const Finalizer = "training.dcnlab.com/framework-cleanup"

type ObjectRef struct {
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

type RunSpec struct {
	WorkloadRef        ObjectRef          `json:"workloadRef"`
	OnPremiseCluster   string             `json:"onPremiseCluster"`
	WorldSize          int64              `json:"worldSize"`
	WaitSeconds        *int64             `json:"waitSeconds,omitempty"`
	PreparationSeconds *int64             `json:"preparationSeconds,omitempty"`
	CheckpointSeconds  int64              `json:"checkpointSeconds"`
	RuntimePort        int64              `json:"runtimePort,omitempty"`
	Container          string             `json:"container,omitempty"`
	ExecutionGate      bool               `json:"executionGate,omitempty"`
	Suspend            bool               `json:"suspend,omitempty"`
	ResourcePolicy     ResourcePolicySpec `json:"resourcePolicy,omitempty"`
	Failover           FailoverSpec       `json:"failover,omitempty"`
	// Observe is the default for staged rollout. Execute is an explicit user
	// intent, not an annotation silently enabling mutations on old workloads.
	Mode  string    `json:"mode"`
	Cloud CloudSpec `json:"cloud"`
}

// Failover is an explicit opt-in separate from Cloud overflow permission.
// Grace and repeated failures avoid migrating on transient runtime observations.
type FailoverSpec struct {
	Enabled             bool  `json:"enabled"`
	FailureGraceSeconds int64 `json:"failureGraceSeconds,omitempty"`
	RestartThreshold    int64 `json:"restartThreshold,omitempty"`
}

func (f FailoverSpec) limits() (time.Duration, int64) {
	grace, restarts := f.FailureGraceSeconds, f.RestartThreshold
	if grace == 0 {
		grace = 60
	}
	if restarts == 0 {
		restarts = 3
	}
	return time.Duration(grace) * time.Second, restarts
}

// ResourcePolicySpec configures the existing paper-eq6-7 policy. It is part of
// the training request, not a replacement for Karmada placement policy.
type ResourcePolicySpec struct {
	Alpha           *float64 `json:"alpha,omitempty"`
	MinOnDemand     int64    `json:"minOnDemand,omitempty"`
	ForecastSeconds int64    `json:"forecastSeconds,omitempty"`
}

type CloudSpec struct {
	// nil preserves the workload runtime; empty selects the member's default
	// runtime (for example, a reviewed CRI-O/CDI installation).
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`
	Allowed          bool    `json:"allowed"`
	Cluster          string  `json:"cluster,omitempty"`
	TemplateRef      string  `json:"templateRef,omitempty"`
	RiskProfileRef   string  `json:"riskProfileRef,omitempty"`
	MaxWorkers       int64   `json:"maxWorkers,omitempty"`
	// Every paid allocation has a precommitted absolute stop deadline.
	Deadline string `json:"deadline,omitempty"`
}

type Allocation struct {
	FenceOperationUID string `json:"fenceOperationUID,omitempty"`
	Name              string `json:"name"`
	Market            string `json:"market"`
	State             string `json:"state"` // Prepare, Retain, Fence, Retire
}

type CapacitySpec struct {
	RunRef      ObjectRef    `json:"runRef"`
	Cluster     string       `json:"cluster"`
	TemplateRef string       `json:"templateRef"`
	MaxWorkers  int64        `json:"maxWorkers"`
	Deadline    string       `json:"deadline"`
	Allocations []Allocation `json:"allocations"`
}

func decodeSpec(o *unstructured.Unstructured, into interface{}) error {
	b, err := json.Marshal(o.Object["spec"])
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}
func encode(v interface{}) map[string]interface{} {
	value := reflect.ValueOf(v)
	if value.Kind() != reflect.Ptr {
		ptr := reflect.New(value.Type())
		ptr.Elem().Set(value)
		v = ptr.Interface()
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(v)
	if err != nil {
		panic(err)
	}
	return m
}

func object(apiVersion, kind string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	return u
}
func objects(apiVersion, kind string) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetAPIVersion(apiVersion)
	l.SetKind(kind + "List")
	return l
}
func owned(child, parent *unstructured.Unstructured) bool {
	for _, o := range child.GetOwnerReferences() {
		if o.UID == parent.GetUID() && o.Name == parent.GetName() && o.Kind == parent.GetKind() && o.APIVersion == parent.GetAPIVersion() {
			return true
		}
	}
	return false
}
func setOwner(child, parent *unstructured.Unstructured) {
	controller := true
	child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: parent.GetAPIVersion(), Kind: parent.GetKind(), Name: parent.GetName(), UID: parent.GetUID(), Controller: &controller}})
}

// ensureSpec uses an optimistic resourceVersion and never adopts a name collision.
func ensureSpec(ctx context.Context, c client.Client, desired, parent *unstructured.Unstructured) error {
	if parent.GetUID() == "" {
		return fmt.Errorf("persisted owner UID required")
	}
	setOwner(desired, parent)
	old := object(desired.GetAPIVersion(), desired.GetKind())
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), old)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !owned(old, parent) || !old.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("%s %s ownership conflict or deletion in progress", desired.GetKind(), desired.GetName())
	}
	// Retain API-defaulted fields outside this controller's explicit contract.
	// Removing those on every poll would cause needless policy propagation.
	merged, _, _ := unstructured.NestedMap(old.Object, "spec")
	if merged == nil {
		merged = map[string]interface{}{}
	}
	overlaySpec(merged, desired.Object["spec"].(map[string]interface{}))
	if reflect.DeepEqual(old.Object["spec"], merged) {
		return nil
	}
	base := old.DeepCopy()
	old.Object["spec"] = merged
	return c.Patch(ctx, old, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func overlaySpec(dst, src map[string]interface{}) {
	for k, v := range src {
		if child, ok := v.(map[string]interface{}); ok {
			if existing, ok := dst[k].(map[string]interface{}); ok {
				overlaySpec(existing, child)
				continue
			}
		}
		dst[k] = runtime.DeepCopyJSONValue(v)
	}
}
func named(kind, ns, name string) *unstructured.Unstructured {
	o := resource.Object(kind)
	o.SetNamespace(ns)
	o.SetName(name)
	return o
}
func str(o *unstructured.Unstructured, path ...string) string    { return resource.String(o, path...) }
func integer(o *unstructured.Unstructured, path ...string) int64 { return resource.Int(o, path...) }
func stamp(t time.Time) string                                   { return t.UTC().Format(time.RFC3339Nano) }
func boolAt(o *unstructured.Unstructured, path ...string) bool {
	b, _, _ := unstructured.NestedBool(o.Object, path...)
	return b
}
func listItems(ctx context.Context, c client.Reader, api, kind, ns string) ([]unstructured.Unstructured, error) {
	l := objects(api, kind)
	err := c.List(ctx, l, client.InNamespace(ns))
	return l.Items, err
}
func sameWorkload(o *unstructured.Unstructured, name, uid string) bool {
	return str(o, "spec", "workloadRef", "name") == name && str(o, "spec", "workloadRef", "uid") == uid
}
func key(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}
func gvk(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: resource.Group, Version: "v1alpha1", Kind: kind}
}

func (s RunSpec) Validate() error {
	if s.Failover.Enabled && !s.Cloud.Allowed {
		return fmt.Errorf("failover requires approved Cloud target")
	}
	if s.Failover.FailureGraceSeconds < 0 || s.Failover.FailureGraceSeconds > 86400 || s.Failover.RestartThreshold < 0 || s.Failover.RestartThreshold > 100 {
		return fmt.Errorf("invalid failure confirmation limits")
	}
	if s.WorkloadRef.Name == "" || s.OnPremiseCluster == "" || s.WorldSize < 1 || s.WorldSize > 128 || s.CheckpointSeconds <= 0 {
		return fmt.Errorf("workload, cluster, worldSize and checkpointSeconds are required")
	}
	if s.Mode != "Observe" && s.Mode != "Execute" {
		return fmt.Errorf("mode must be Observe or Execute")
	}
	if s.ResourcePolicy.Alpha != nil && (math.IsNaN(*s.ResourcePolicy.Alpha) || math.IsInf(*s.ResourcePolicy.Alpha, 0) || *s.ResourcePolicy.Alpha <= 0 || *s.ResourcePolicy.Alpha > 1) {
		return fmt.Errorf("resourcePolicy.alpha must be in (0,1]")
	}
	if s.ResourcePolicy.MinOnDemand < 0 || s.ResourcePolicy.MinOnDemand > s.WorldSize || s.ResourcePolicy.ForecastSeconds < 0 || s.ResourcePolicy.ForecastSeconds > 86400 {
		return fmt.Errorf("invalid resourcePolicy worker minimum or forecast horizon")
	}
	w, p := s.Wait()
	if _, err := PreparationAt(time.Now(), w, p); err != nil {
		return err
	}
	if s.Cloud.Allowed && (s.Cloud.Cluster == "" || s.Cloud.Cluster == s.OnPremiseCluster || s.Cloud.TemplateRef == "" || s.Cloud.RiskProfileRef == "" || s.Cloud.MaxWorkers < s.WorldSize) {
		return fmt.Errorf("cloud cluster, reviewed template, risk profile and capacity limit required")
	}
	if s.Cloud.Allowed && s.Mode == "Execute" && !s.ExecutionGate {
		return fmt.Errorf("Cloud execution requires a Pod-UID execution gate")
	}
	if s.Cloud.Allowed {
		if _, err := time.Parse(time.RFC3339, s.Cloud.Deadline); err != nil {
			return fmt.Errorf("absolute cloud deadline required")
		}
	}
	return nil
}
func (s RunSpec) Wait() (int64, int64) {
	w, p := DefaultWaitSeconds, DefaultPreparationSeconds
	if s.WaitSeconds != nil {
		w = *s.WaitSeconds
	}
	if s.PreparationSeconds != nil {
		p = *s.PreparationSeconds
	}
	return w, p
}

// containsSpec verifies every explicit field including false/zero values while
// accepting additional API-defaulted keys. It never ignores a Boolean mismatch.
func containsSpec(actual, expected interface{}) bool {
	switch want := expected.(type) {
	case map[string]interface{}:
		got, ok := actual.(map[string]interface{})
		if !ok {
			return false
		}
		for k, v := range want {
			a, exists := got[k]
			if !exists || !containsSpec(a, v) {
				return false
			}
		}
		return true
	case []interface{}:
		got, ok := actual.([]interface{})
		if !ok || len(got) != len(want) {
			return false
		}
		for i, v := range want {
			if !containsSpec(got[i], v) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(actual, expected)
	}
}
