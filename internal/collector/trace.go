package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Availability decreases are a declared experimental proxy, not observed evictions.
type availabilityTrace struct {
	Metadata struct {
		GapSeconds float64 `json:"gap_seconds"`
	} `json:"metadata"`
	Data []*float64 `json:"data"`
}

func estimateAvailability(body []byte, end, window int64) (float64, map[string]interface{}, error) {
	if len(body) > 1024*1024 {
		return 0, nil, fmt.Errorf("trace exceeds 1 MiB")
	}
	var trace availabilityTrace
	if err := json.Unmarshal(body, &trace); err != nil {
		return 0, nil, fmt.Errorf("invalid trace JSON")
	}
	gap := trace.Metadata.GapSeconds
	if gap <= 0 || math.IsNaN(gap) || math.IsInf(gap, 0) {
		return 0, nil, fmt.Errorf("metadata.gap_seconds must be finite and positive")
	}
	if len(trace.Data) > 100000 || window < 2 || window > 100000 || end < 1 || end >= int64(len(trace.Data)) || window > end+1 {
		return 0, nil, fmt.Errorf("trace needs a complete windowSamples >= 2 ending at a valid zero-based endIndex")
	}
	start := end - window + 1
	for i := start; i <= end; i++ {
		if trace.Data[i] == nil {
			return 0, nil, fmt.Errorf("sample %d is null", i)
		}
		v := *trace.Data[i]
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v {
			return 0, nil, fmt.Errorf("sample %d must be a nonnegative integer", i)
		}
	}
	loss, exposure := 0.0, 0.0
	for i := start; i < end; i++ {
		exposure += *trace.Data[i] * gap / 3600
		loss += math.Max(*trace.Data[i]-*trace.Data[i+1], 0)
	}
	if exposure <= 0 || math.IsInf(exposure, 0) || math.IsInf(loss, 0) {
		return 0, nil, fmt.Errorf("trace window has no finite positive instance-hour exposure")
	}
	lambda := loss / exposure
	if err := validateFiniteNonNegative("estimated lambda", lambda); err != nil {
		return 0, nil, err
	}
	digest := sha256.Sum256(body)
	return lambda, map[string]interface{}{
		"type": "trace", "provenance": "historical-availability-proxy", "experimental": true,
		"estimator":   "positive-decreases-per-instance-hour",
		"traceSHA256": hex.EncodeToString(digest[:]), "startIndex": start, "endIndex": end,
		"windowSamples": window, "gapSeconds": gap, "downwardUnits": loss, "exposureInstanceHours": exposure,
	}, nil
}

func (r *Reconciler) collectTrace(ctx context.Context, obj *unstructured.Unstructured, now time.Time, validity time.Duration) (normalizedRisk, error) {
	allowed, _, _ := unstructured.NestedBool(obj.Object, "spec", "trace", "allowAvailabilityProxy")
	if !allowed {
		return normalizedRisk{}, fmt.Errorf("availability proxy requires explicit allowAvailabilityProxy=true")
	}
	name := resource.String(obj, "spec", "trace", "configMapRef", "name")
	key := resource.String(obj, "spec", "trace", "configMapRef", "key")
	if name == "" || key == "" {
		return normalizedRisk{}, fmt.Errorf("trace configMapRef name and key are required")
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if reader == nil {
		return normalizedRisk{}, fmt.Errorf("ConfigMap reader is not configured")
	}
	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, cm); err != nil {
		return normalizedRisk{}, fmt.Errorf("read trace ConfigMap: %w", err)
	}
	body, ok := cm.Data[key]
	if !ok || body == "" {
		return normalizedRisk{}, fmt.Errorf("trace ConfigMap data key is empty or missing")
	}
	end, _, _ := unstructured.NestedInt64(obj.Object, "spec", "trace", "endIndex")
	window, _, _ := unstructured.NestedInt64(obj.Object, "spec", "trace", "windowSamples")
	lambda, source, err := estimateAvailability([]byte(body), end, window)
	if err != nil {
		return normalizedRisk{}, err
	}
	source["configMapName"] = name
	source["configMapUID"] = string(cm.UID)
	source["configMapResourceVersion"] = cm.ResourceVersion
	source["configMapKey"] = key
	return normalizedRisk{LambdaPerHour: lambda, ObservedAt: now, ValidUntil: now.Add(validity), Source: source}, nil
}
