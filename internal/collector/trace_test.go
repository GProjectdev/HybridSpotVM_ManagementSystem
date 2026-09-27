package collector

import (
	"context"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"math"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestAvailabilityEstimator(t *testing.T) {
	tests := []struct {
		name, body  string
		end, window int64
		want        float64
		bad         bool
	}{
		{"hand calculation", `{"metadata":{"gap_seconds":3600},"data":[4,2,3]}`, 2, 3, 1.0 / 3, false},
		{"window excludes earlier loss", `{"metadata":{"gap_seconds":3600},"data":[4,2,3]}`, 2, 2, 0, false},
		{"no future leakage", `{"metadata":{"gap_seconds":3600},"data":[4,2,0]}`, 1, 2, .5, false},
		{"zero exposure", `{"metadata":{"gap_seconds":300},"data":[0,0]}`, 1, 2, 0, true},
		{"bad gap", `{"metadata":{"gap_seconds":0},"data":[4,2]}`, 1, 2, 0, true},
		{"negative", `{"metadata":{"gap_seconds":300},"data":[4,-1]}`, 1, 2, 0, true},
		{"null", `{"metadata":{"gap_seconds":300},"data":[4,null]}`, 1, 2, 0, true},
		{"fraction", `{"metadata":{"gap_seconds":300},"data":[4,1.5]}`, 1, 2, 0, true},
		{"missing data", `{}`, 1, 2, 0, true},
		{"outside", `{"metadata":{"gap_seconds":300},"data":[4,2]}`, 2, 2, 0, true},
		{"short window", `{"metadata":{"gap_seconds":300},"data":[4,2]}`, 1, 3, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := estimateAvailability([]byte(tt.body), tt.end, tt.window)
			if (err != nil) != tt.bad {
				t.Fatalf("error = %v", err)
			}
			if !tt.bad && math.Abs(got-tt.want) > 1e-10 {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestLocalSkyPilotTrace(t *testing.T) {
	path := os.Getenv("SPOT_TRACE_FILE")
	if path == "" {
		t.Skip("optional local historical fixture")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, source, err := estimateAvailability(body, 4735, 4736)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-0.506848891499949) > 1e-10 {
		t.Fatalf("unexpected full trace estimate: %v", got)
	}
	t.Logf("lambda=%v evidence=%v", got, source)
}

func TestTraceConfigMapCollection(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "trace", Namespace: "demo"}, Data: map[string]string{"trace.json": `{"metadata":{"gap_seconds":3600},"data":[4,2,3]}`}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	r := &Reconciler{Client: c, APIReader: c, Clock: func() time.Time { return time.Unix(1000, 0) }}
	obj := resource.Object("SpotRiskProfile")
	obj.SetNamespace("demo")
	obj.Object["spec"] = map[string]interface{}{"trace": map[string]interface{}{
		"configMapRef":           map[string]interface{}{"name": "trace", "key": "trace.json"},
		"allowAvailabilityProxy": true, "windowSamples": int64(3), "endIndex": int64(2),
	}}
	risk, err := r.collect(context.Background(), obj)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(risk.LambdaPerHour-1.0/3) > 1e-10 || risk.Source["experimental"] != true || risk.Source["configMapName"] != "trace" {
		t.Fatalf("unexpected risk: %#v", risk)
	}
	obj.Object["spec"].(map[string]interface{})["staticLambdaPerHour"] = .1
	if _, err := r.collect(context.Background(), obj); err == nil {
		t.Fatal("mixed inputs accepted")
	}
	delete(obj.Object["spec"].(map[string]interface{}), "staticLambdaPerHour")
	obj.SetNamespace("other")
	if _, err := r.collect(context.Background(), obj); err == nil {
		t.Fatal("cross namespace read accepted")
	}
	obj.SetNamespace("demo")
	obj.Object["spec"].(map[string]interface{})["trace"].(map[string]interface{})["allowAvailabilityProxy"] = false
	if _, err := r.collect(context.Background(), obj); err == nil {
		t.Fatal("implicit proxy accepted")
	}
}
