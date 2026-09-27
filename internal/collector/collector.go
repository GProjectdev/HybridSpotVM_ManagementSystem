package collector

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
)

const (
	spotRiskProfileKind = "SpotRiskProfile"
	defaultMaxAge       = time.Hour
	defaultPoll         = 5 * time.Minute
	defaultHTTPTimeout  = 10 * time.Second
	maxResponseBytes    = 64 * 1024
	defaultSecretKey    = "token"
	staticSourceType    = "static"
	endpointSourceType  = "https"
	paperSourceType     = "paper-sarima"
)

var spotRiskProfileGVK = schema.GroupVersionKind{
	Group:   resource.Group,
	Version: "v1alpha1",
	Kind:    spotRiskProfileKind,
}

type Reconciler struct {
	client.Client
	APIReader  client.Reader
	HTTPClient *http.Client
	Clock      func() time.Time
}

type profileSpec struct {
	Endpoint            string
	PaperEndpoint       string
	StaticLambdaPerHour *float64
	MaxAge              time.Duration
	Poll                time.Duration
	Credential          credentialRef
}

type credentialRef struct {
	Name      string
	Namespace string
	Key       string
}

type feedResponse struct {
	LambdaPerHour        *float64               `json:"lambdaPerHour"`
	ObservedAt           string                 `json:"observedAt"`
	ValidUntil           string                 `json:"validUntil"`
	SpotPricePerHour     *float64               `json:"spotPricePerHour,omitempty"`
	OnDemandPricePerHour *float64               `json:"onDemandPricePerHour,omitempty"`
	Source               map[string]interface{} `json:"source,omitempty"`
}

type normalizedRisk struct {
	LambdaPerHour        float64
	ObservedAt           time.Time
	ValidUntil           time.Time
	SpotPricePerHour     *float64
	OnDemandPricePerHour *float64
	Source               map[string]interface{}
}

func Setup(mgr ctrl.Manager) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(spotRiskProfileGVK)

	return ctrl.NewControllerManagedBy(mgr).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		For(obj).
		Complete(&Reconciler{
			Client:     mgr.GetClient(),
			APIReader:  mgr.GetAPIReader(),
			HTTPClient: defaultHTTPClient(),
			Clock:      time.Now,
		})
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := resource.Object(spotRiskProfileKind)
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	poll := parseSpec(obj).Poll
	if poll <= 0 {
		poll = defaultPoll
	}

	risk, err := r.collect(ctx, obj)
	if err != nil {
		status := map[string]interface{}{
			"observedGeneration": obj.GetGeneration(),
			"ready":              false,
			"error":              sanitizeStatusError(err),
		}
		if setErr := resource.SetStatus(ctx, r.Client, obj, status); setErr != nil {
			return ctrl.Result{}, setErr
		}
		return ctrl.Result{RequeueAfter: poll}, nil
	}

	status := map[string]interface{}{
		"observedGeneration": obj.GetGeneration(),
		"lambdaPerHour":      risk.LambdaPerHour,
		"observedAt":         resource.Timestamp(risk.ObservedAt),
		"validUntil":         resource.Timestamp(risk.ValidUntil),
		"ready":              true,
		"source":             risk.Source,
	}
	prices := map[string]interface{}{}
	if risk.SpotPricePerHour != nil {
		prices["spotPricePerHour"] = *risk.SpotPricePerHour
	}
	if risk.OnDemandPricePerHour != nil {
		prices["onDemandPricePerHour"] = *risk.OnDemandPricePerHour
	}
	if len(prices) > 0 {
		status["prices"] = prices
	}

	if err := resource.SetStatus(ctx, r.Client, obj, status); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: poll}, nil
}

func (r *Reconciler) collect(ctx context.Context, obj *unstructured.Unstructured) (normalizedRisk, error) {
	spec := parseSpec(obj)
	now := r.now()
	_, hasTrace, _ := unstructured.NestedMap(obj.Object, "spec", "trace")
	_, hasPaperEstimator, _ := unstructured.NestedMap(obj.Object, "spec", "paperEstimator")
	sources := 0
	if hasTrace {
		sources++
	}
	if hasPaperEstimator {
		sources++
	}
	if spec.Endpoint != "" {
		sources++
	}
	if spec.StaticLambdaPerHour != nil {
		sources++
	}
	if sources != 1 {
		return normalizedRisk{}, errors.New("exactly one of trace, endpoint, staticLambdaPerHour, or paperEstimator is required")
	}
	if hasTrace {
		return r.collectTrace(ctx, obj, now, spec.MaxAge)
	}
	if hasPaperEstimator {
		if spec.PaperEndpoint == "" {
			return normalizedRisk{}, errors.New("spec.paperEstimator.feedEndpoint is required")
		}
		token, err := r.bearerToken(ctx, obj.GetNamespace(), spec.Credential)
		if err != nil {
			return normalizedRisk{}, err
		}
		risk, err := fetchEndpointRisk(ctx, r.httpClient(), spec.PaperEndpoint, token, spec.MaxAge, now)
		if err != nil {
			return normalizedRisk{}, err
		}
		if err := validatePaperRiskSource(obj, &risk); err != nil {
			return normalizedRisk{}, err
		}
		return risk, nil
	}

	if spec.StaticLambdaPerHour != nil {
		if err := validateFiniteNonNegative("staticLambdaPerHour", *spec.StaticLambdaPerHour); err != nil {
			return normalizedRisk{}, err
		}
		return normalizedRisk{
			LambdaPerHour: *spec.StaticLambdaPerHour,
			ObservedAt:    now,
			ValidUntil:    now.Add(spec.Poll),
			Source: map[string]interface{}{
				"type":       staticSourceType,
				"provenance": "static-experiment-spec",
			},
		}, nil
	}

	if spec.Endpoint == "" {
		return normalizedRisk{}, errors.New("spec.endpoint or spec.staticLambdaPerHour is required")
	}

	token, err := r.bearerToken(ctx, obj.GetNamespace(), spec.Credential)
	if err != nil {
		return normalizedRisk{}, err
	}
	return fetchEndpointRisk(ctx, r.httpClient(), spec.Endpoint, token, spec.MaxAge, now)
}

func validatePaperRiskSource(obj *unstructured.Unstructured, risk *normalizedRisk) error {
	feedSource, ok := risk.Source["feed"].(map[string]interface{})
	if !ok || len(feedSource) == 0 {
		return errors.New("paperEstimator feed source metadata is required")
	}
	if sourceType, _ := feedSource["type"].(string); sourceType != paperSourceType {
		return fmt.Errorf("paperEstimator feed source.type = %q, want %q", sourceType, paperSourceType)
	}
	if provenance, _ := feedSource["provenance"].(string); provenance != "paper-availability-count-sarima" {
		return errors.New("paperEstimator feed provenance must be paper-availability-count-sarima")
	}
	if err := requirePaperParam(feedSource, "rollingMeanHours", paperParam(obj, 3, "rollingMeanHours")); err != nil {
		return err
	}
	if err := requirePaperParam(feedSource, "retrainWindowWeeks", paperParam(obj, 4, "retrainWindowWeeks")); err != nil {
		return err
	}
	if err := requirePaperParam(feedSource, "seasonalPeriodHours", paperParam(obj, 24, "seasonalPeriodHours")); err != nil {
		return err
	}
	riskPopulation, ok := sourceNumber(feedSource, "riskPopulation")
	if !ok || riskPopulation <= 0 || !finiteNumber(riskPopulation) {
		return errors.New("paperEstimator feed source.riskPopulation must be finite and positive")
	}
	aggregate, ok := sourceNumber(feedSource, "aggregateForecastPreemptionsPerHour")
	if !ok || aggregate < 0 || !finiteNumber(aggregate) {
		return errors.New("paperEstimator feed source.aggregateForecastPreemptionsPerHour must be finite and nonnegative")
	}
	approx, _ := feedSource["perInstanceLambdaApproximation"].(string)
	if approx == "" {
		return errors.New("paperEstimator feed source.perInstanceLambdaApproximation is required")
	}
	if risk.LambdaPerHour > aggregate/riskPopulation+1e-9 {
		return errors.New("paperEstimator lambdaPerHour exceeds aggregateForecastPreemptionsPerHour/riskPopulation")
	}
	if host, ok := risk.Source["host"].(string); ok && host != "" {
		feedSource["host"] = host
	}
	risk.Source = feedSource
	return nil
}

func paperParam(obj *unstructured.Unstructured, fallback int64, field string) int64 {
	value := resource.Int(obj, "spec", "paperEstimator", field)
	if value <= 0 {
		return fallback
	}
	return value
}

func requirePaperParam(source map[string]interface{}, field string, expected int64) error {
	actual, ok := sourceNumber(source, field)
	if !ok || !finiteNumber(actual) || actual != float64(expected) {
		return fmt.Errorf("paperEstimator feed source.%s = %v, want %d", field, source[field], expected)
	}
	return nil
}

func sourceNumber(source map[string]interface{}, field string) (float64, bool) {
	switch value := source[field].(type) {
	case float64:
		return value, true
	case int64:
		return float64(value), true
	case int:
		return float64(value), true
	case json.Number:
		parsed, err := value.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func finiteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func parseSpec(obj *unstructured.Unstructured) profileSpec {
	maxAge := secondsDuration(obj, defaultMaxAge, "spec", "maxAgeSeconds")
	poll := secondsDuration(obj, defaultPoll, "spec", "pollSeconds")
	spec := profileSpec{
		Endpoint:      resource.String(obj, "spec", "endpoint"),
		PaperEndpoint: resource.String(obj, "spec", "paperEstimator", "feedEndpoint"),
		MaxAge:        maxAge,
		Poll:          poll,
	}
	if v, ok := nestedNumber(obj, "spec", "staticLambdaPerHour"); ok {
		spec.StaticLambdaPerHour = &v
	}
	if name := resource.String(obj, "spec", "credentialSecretRef", "name"); name != "" {
		spec.Credential.Name = name
		spec.Credential.Namespace = resource.String(obj, "spec", "credentialSecretRef", "namespace")
		spec.Credential.Key = resource.String(obj, "spec", "credentialSecretRef", "key")
		if spec.Credential.Key == "" {
			spec.Credential.Key = defaultSecretKey
		}
	}
	return spec
}

func nestedNumber(obj *unstructured.Unstructured, fields ...string) (float64, bool) {
	if v, ok, _ := unstructured.NestedFloat64(obj.Object, fields...); ok {
		return v, true
	}
	if v, ok, _ := unstructured.NestedInt64(obj.Object, fields...); ok {
		return float64(v), true
	}
	return 0, false
}

func secondsDuration(obj *unstructured.Unstructured, fallback time.Duration, fields ...string) time.Duration {
	seconds := resource.Int(obj, fields...)
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func (r *Reconciler) bearerToken(ctx context.Context, profileNamespace string, ref credentialRef) (string, error) {
	if ref.Name == "" {
		return "", nil
	}
	if ref.Namespace != "" && ref.Namespace != profileNamespace {
		return "", errors.New("credentialSecretRef must reference a Secret in the SpotRiskProfile namespace")
	}

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if reader == nil {
		return "", errors.New("credential Secret reader is not configured")
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: profileNamespace, Name: ref.Name}
	if err := reader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("credential Secret %q not found in namespace %q", ref.Name, profileNamespace)
		}
		return "", fmt.Errorf("read credential Secret: %w", err)
	}
	token := string(secret.Data[ref.Key])
	if token == "" {
		return "", fmt.Errorf("credential Secret key %q is empty or missing", ref.Key)
	}
	return token, nil
}

func fetchEndpointRisk(ctx context.Context, hc *http.Client, endpoint, bearerToken string, maxAge time.Duration, now time.Time) (normalizedRisk, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return normalizedRisk{}, fmt.Errorf("parse endpoint: %w", err)
	}
	if parsed.Scheme != "https" {
		return normalizedRisk{}, errors.New("spec.endpoint must use https")
	}
	if parsed.Host == "" {
		return normalizedRisk{}, errors.New("spec.endpoint must include a host")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return normalizedRisk{}, fmt.Errorf("build feed request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return normalizedRisk{}, fmt.Errorf("fetch risk feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return normalizedRisk{}, fmt.Errorf("risk feed returned HTTP %d", resp.StatusCode)
	}

	body, err := readLimited(resp.Body, maxResponseBytes)
	if err != nil {
		return normalizedRisk{}, err
	}
	var raw feedResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return normalizedRisk{}, fmt.Errorf("decode risk feed JSON: %w", err)
	}
	return normalizeFeed(raw, parsed.Host, maxAge, now)
}

func normalizeFeed(raw feedResponse, endpointHost string, maxAge time.Duration, now time.Time) (normalizedRisk, error) {
	if raw.LambdaPerHour == nil {
		return normalizedRisk{}, errors.New("lambdaPerHour is required")
	}
	if err := validateFiniteNonNegative("lambdaPerHour", *raw.LambdaPerHour); err != nil {
		return normalizedRisk{}, err
	}
	observedAt, err := time.Parse(time.RFC3339, raw.ObservedAt)
	if err != nil {
		return normalizedRisk{}, fmt.Errorf("observedAt must be RFC3339: %w", err)
	}
	validUntil, err := time.Parse(time.RFC3339, raw.ValidUntil)
	if err != nil {
		return normalizedRisk{}, fmt.Errorf("validUntil must be RFC3339: %w", err)
	}
	if observedAt.After(now.Add(time.Minute)) {
		return normalizedRisk{}, errors.New("observedAt is too far in the future")
	}
	if now.Sub(observedAt) > maxAge {
		return normalizedRisk{}, errors.New("risk feed observation is stale")
	}
	if !validUntil.After(now) {
		return normalizedRisk{}, errors.New("risk feed validity has expired")
	}
	if raw.SpotPricePerHour != nil {
		if err := validateFiniteNonNegative("spotPricePerHour", *raw.SpotPricePerHour); err != nil {
			return normalizedRisk{}, err
		}
	}
	if raw.OnDemandPricePerHour != nil {
		if err := validateFiniteNonNegative("onDemandPricePerHour", *raw.OnDemandPricePerHour); err != nil {
			return normalizedRisk{}, err
		}
	}

	return normalizedRisk{
		LambdaPerHour:        *raw.LambdaPerHour,
		ObservedAt:           observedAt.UTC(),
		ValidUntil:           validUntil.UTC(),
		SpotPricePerHour:     raw.SpotPricePerHour,
		OnDemandPricePerHour: raw.OnDemandPricePerHour,
		Source:               endpointSource(endpointHost, raw.Source),
	}, nil
}

func endpointSource(endpointHost string, feedSource map[string]interface{}) map[string]interface{} {
	source := map[string]interface{}{
		"type": endpointSourceType,
		"host": endpointHost,
	}
	if len(feedSource) > 0 {
		source["feed"] = feedSource
	}
	return source
}

func validateFiniteNonNegative(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%s must be finite", name)
	}
	if value < 0 {
		return fmt.Errorf("%s must be nonnegative", name)
	}
	return nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(r, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read risk feed response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("risk feed response exceeds %d bytes", limit)
	}
	return body, nil
}

func (r *Reconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return defaultHTTPClient()
}

func (r *Reconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock().UTC()
	}
	return time.Now().UTC()
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func sanitizeStatusError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 256 {
		return msg[:256]
	}
	return msg
}
