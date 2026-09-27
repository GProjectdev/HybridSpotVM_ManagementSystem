package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
)

func TestFetchEndpointRiskAcceptsTLSFeed(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	observedAt := now.Add(-2 * time.Minute).Format(time.RFC3339)
	validUntil := now.Add(30 * time.Minute).Format(time.RFC3339)
	var gotAuth string

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"lambdaPerHour":0.125,"observedAt":%q,"validUntil":%q,"spotPricePerHour":0.017,"onDemandPricePerHour":0.05}`, observedAt, validUntil)
	}))
	defer server.Close()

	risk, err := fetchEndpointRisk(context.Background(), server.Client(), server.URL, "secret-token", time.Hour, now)
	if err != nil {
		t.Fatalf("fetchEndpointRisk() error = %v", err)
	}
	if risk.LambdaPerHour != 0.125 {
		t.Fatalf("lambdaPerHour = %v, want 0.125", risk.LambdaPerHour)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuth)
	}
	if risk.Source["type"] != endpointSourceType || risk.Source["host"] == "" {
		t.Fatalf("source = %#v, want endpoint identity", risk.Source)
	}
	if risk.SpotPricePerHour == nil || *risk.SpotPricePerHour != 0.017 {
		t.Fatalf("spot price = %#v, want 0.017", risk.SpotPricePerHour)
	}
}

func TestFetchEndpointRiskRejectsRedirects(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	redirected := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected = true
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	client := server.Client()
	client.CheckRedirect = defaultHTTPClient().CheckRedirect
	_, err := fetchEndpointRisk(context.Background(), client, server.URL, "do-not-leak", time.Hour, now)
	if err == nil {
		t.Fatal("fetchEndpointRisk() error = nil, want redirect rejection")
	}
	if redirected {
		t.Fatal("redirect target was called; credentials could leak")
	}
}

func TestNormalizeFeedRejectsInvalidHazardAndStaleObservations(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	validUntil := now.Add(time.Hour).Format(time.RFC3339)

	tests := []struct {
		name string
		raw  feedResponse
		want string
	}{
		{
			name: "negative lambda",
			raw: feedResponse{
				LambdaPerHour: float64Ptr(-0.1),
				ObservedAt:    now.Format(time.RFC3339),
				ValidUntil:    validUntil,
			},
			want: "nonnegative",
		},
		{
			name: "stale observation",
			raw: feedResponse{
				LambdaPerHour: float64Ptr(0.1),
				ObservedAt:    now.Add(-2 * time.Hour).Format(time.RFC3339),
				ValidUntil:    validUntil,
			},
			want: "stale",
		},
		{
			name: "expired validity",
			raw: feedResponse{
				LambdaPerHour: float64Ptr(0.1),
				ObservedAt:    now.Format(time.RFC3339),
				ValidUntil:    now.Add(-time.Minute).Format(time.RFC3339),
			},
			want: "expired",
		},
		{
			name: "missing lambda",
			raw: feedResponse{
				ObservedAt: now.Format(time.RFC3339),
				ValidUntil: validUntil,
			},
			want: "required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeFeed(tt.raw, "feed.example.test", time.Hour, now)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("normalizeFeed() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCollectStaticRiskMarksStaticProvenance(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	obj := resource.Object(spotRiskProfileKind)
	obj.SetNamespace("default")
	obj.Object["spec"] = map[string]interface{}{
		"staticLambdaPerHour": int64(0),
		"pollSeconds":         int64(60),
	}
	r := &Reconciler{Clock: func() time.Time { return now }}

	risk, err := r.collect(context.Background(), obj)
	if err != nil {
		t.Fatalf("collect() error = %v", err)
	}
	if risk.LambdaPerHour != 0 {
		t.Fatalf("lambdaPerHour = %v, want 0", risk.LambdaPerHour)
	}
	if risk.Source["type"] != staticSourceType || risk.Source["provenance"] != "static-experiment-spec" {
		t.Fatalf("source = %#v, want static provenance", risk.Source)
	}
}

func float64Ptr(v float64) *float64 {
	return &v
}

func TestCollectSecretMustStayInProfileNamespace(t *testing.T) {
	obj := resource.Object(spotRiskProfileKind)
	obj.SetNamespace("tenant-a")
	obj.Object["spec"] = map[string]interface{}{
		"endpoint": "https://risk.example.test/feed",
		"credentialSecretRef": map[string]interface{}{
			"name":      "risk-token",
			"namespace": "tenant-b",
		},
	}
	r := &Reconciler{}

	_, err := r.collect(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("collect() error = %v, want namespace rejection", err)
	}
}

func TestBearerTokenReadsNamespacedSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "risk-token", Namespace: "tenant-a"},
		Data:       map[string][]byte{"token": []byte("abc123")},
	}
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
	}

	token, err := r.bearerToken(context.Background(), "tenant-a", credentialRef{Name: "risk-token", Key: "token"})
	if err != nil {
		t.Fatalf("bearerToken() error = %v", err)
	}
	if token != "abc123" {
		t.Fatalf("token = %q, want abc123", token)
	}
}

func TestReadLimitedRejectsOversizedResponse(t *testing.T) {
	_, err := readLimited(strings.NewReader(strings.Repeat("x", int(maxResponseBytes)+1)), maxResponseBytes)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("readLimited() error = %v, want size limit", err)
	}
}

func TestParseSpecUsesProposedFields(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"endpoint":      "https://risk.example.test/feed",
			"maxAgeSeconds": int64(120),
			"pollSeconds":   int64(30),
			"credentialSecretRef": map[string]interface{}{
				"name": "risk-token",
			},
		},
	}}

	spec := parseSpec(obj)
	if spec.Endpoint != "https://risk.example.test/feed" {
		t.Fatalf("endpoint = %q", spec.Endpoint)
	}
	if spec.MaxAge != 2*time.Minute || spec.Poll != 30*time.Second {
		t.Fatalf("durations = %v/%v, want 2m/30s", spec.MaxAge, spec.Poll)
	}
	if spec.Credential.Name != "risk-token" || spec.Credential.Key != defaultSecretKey {
		t.Fatalf("credential = %#v, want name with default key", spec.Credential)
	}
}

func TestCollectPaperEstimatorFetchesSARIMAFeed(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	observedAt := now.Add(-2 * time.Minute).Format(time.RFC3339)
	validUntil := now.Add(30 * time.Minute).Format(time.RFC3339)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"lambdaPerHour":0.25,"observedAt":%q,"validUntil":%q,"source":{"type":"paper-sarima","provenance":"paper-availability-count-sarima","rollingMeanHours":3,"retrainWindowWeeks":4,"seasonalPeriodHours":24,"riskPopulation":8,"aggregateForecastPreemptionsPerHour":2,"perInstanceLambdaApproximation":"approximate aggregateForecastPreemptionsPerHour/riskPopulation"}}`, observedAt, validUntil)
	}))
	defer server.Close()

	obj := resource.Object(spotRiskProfileKind)
	obj.SetNamespace("default")
	obj.Object["spec"] = map[string]interface{}{
		"paperEstimator": map[string]interface{}{
			"method":              "sarima",
			"feedEndpoint":        server.URL,
			"rollingMeanHours":    int64(3),
			"retrainWindowWeeks":  int64(4),
			"seasonalPeriodHours": int64(24),
		},
	}
	r := &Reconciler{HTTPClient: server.Client(), Clock: func() time.Time { return now }}

	risk, err := r.collect(context.Background(), obj)
	if err != nil {
		t.Fatalf("collect() error = %v", err)
	}
	if risk.LambdaPerHour != 0.25 {
		t.Fatalf("lambdaPerHour = %v, want 0.25", risk.LambdaPerHour)
	}
	if risk.Source["type"] != paperSourceType || risk.Source["provenance"] != "paper-availability-count-sarima" {
		t.Fatalf("source = %#v, want producer paper SARIMA provenance", risk.Source)
	}
	if risk.Source["aggregateForecastPreemptionsPerHour"] != float64(2) || risk.Source["riskPopulation"] != float64(8) {
		t.Fatalf("source = %#v, want preserved aggregate and population metadata", risk.Source)
	}
	if _, ok := risk.Source["method"]; ok {
		t.Fatalf("source = %#v, did not expect fabricated method", risk.Source)
	}
}

func TestCollectPaperEstimatorRejectsPlainOrMismatchedFeeds(t *testing.T) {
	now := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	observedAt := now.Add(-2 * time.Minute).Format(time.RFC3339)
	validUntil := now.Add(30 * time.Minute).Format(time.RFC3339)
	tests := []struct {
		name string
		body string
		want string
	}{
		{"plain feed", fmt.Sprintf(`{"lambdaPerHour":0.25,"observedAt":%q,"validUntil":%q}`, observedAt, validUntil), "source metadata"},
		{"wrong type", fmt.Sprintf(`{"lambdaPerHour":0.25,"observedAt":%q,"validUntil":%q,"source":{"type":"https","provenance":"paper-availability-count-sarima","rollingMeanHours":3,"retrainWindowWeeks":4,"seasonalPeriodHours":24,"riskPopulation":8,"aggregateForecastPreemptionsPerHour":2,"perInstanceLambdaApproximation":"approximate"}}`, observedAt, validUntil), "source.type"},
		{"mismatched rolling", fmt.Sprintf(`{"lambdaPerHour":0.25,"observedAt":%q,"validUntil":%q,"source":{"type":"paper-sarima","provenance":"paper-availability-count-sarima","rollingMeanHours":6,"retrainWindowWeeks":4,"seasonalPeriodHours":24,"riskPopulation":8,"aggregateForecastPreemptionsPerHour":2,"perInstanceLambdaApproximation":"approximate"}}`, observedAt, validUntil), "rollingMeanHours"},
		{"missing population", fmt.Sprintf(`{"lambdaPerHour":0.25,"observedAt":%q,"validUntil":%q,"source":{"type":"paper-sarima","provenance":"paper-availability-count-sarima","rollingMeanHours":3,"retrainWindowWeeks":4,"seasonalPeriodHours":24,"aggregateForecastPreemptionsPerHour":2,"perInstanceLambdaApproximation":"approximate"}}`, observedAt, validUntil), "riskPopulation"},
		{"lambda exceeds normalized aggregate", fmt.Sprintf(`{"lambdaPerHour":0.5,"observedAt":%q,"validUntil":%q,"source":{"type":"paper-sarima","provenance":"paper-availability-count-sarima","rollingMeanHours":3,"retrainWindowWeeks":4,"seasonalPeriodHours":24,"riskPopulation":8,"aggregateForecastPreemptionsPerHour":2,"perInstanceLambdaApproximation":"approximate"}}`, observedAt, validUntil), "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			obj := resource.Object(spotRiskProfileKind)
			obj.SetNamespace("default")
			obj.Object["spec"] = map[string]interface{}{"paperEstimator": map[string]interface{}{"method": "sarima", "feedEndpoint": server.URL, "rollingMeanHours": int64(3), "retrainWindowWeeks": int64(4), "seasonalPeriodHours": int64(24)}}
			r := &Reconciler{HTTPClient: server.Client(), Clock: func() time.Time { return now }}
			_, err := r.collect(context.Background(), obj)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("collect() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCollectPaperEstimatorRequiresFeedEndpoint(t *testing.T) {
	obj := resource.Object(spotRiskProfileKind)
	obj.SetNamespace("default")
	obj.Object["spec"] = map[string]interface{}{
		"paperEstimator": map[string]interface{}{
			"method": "sarima",
		},
	}
	r := &Reconciler{Clock: func() time.Time { return time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC) }}

	_, err := r.collect(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "paperEstimator.feedEndpoint") {
		t.Fatalf("collect() error = %v, want feedEndpoint rejection", err)
	}
}
