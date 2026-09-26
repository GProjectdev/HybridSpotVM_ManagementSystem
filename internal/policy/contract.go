package policy

import (
	"math"
	"sort"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	Group   = "training.dcnlab.com"
	Version = "v1alpha1"

	DefaultAlpha                  = 0.8
	DefaultForecastHorizonSeconds = int64(3600)
	DefaultCheckpointSeconds      = int64(120)
	DefaultControlPort            = int64(8298)
	DefaultAWSCluster             = "aws"

	StatusPolicyPath     = "policy"
	StatusCheckpointPath = "checkpoint"
)

var (
	TrainingPolicyGVK    = schema.GroupVersionKind{Group: Group, Version: Version, Kind: "TrainingPolicy"}
	SpotRiskProfileGVK   = schema.GroupVersionKind{Group: Group, Version: Version, Kind: "SpotRiskProfile"}
	TrainingRuntimeGVK   = schema.GroupVersionKind{Group: Group, Version: Version, Kind: "TrainingRuntime"}
	SpotRecoveryGVK      = schema.GroupVersionKind{Group: Group, Version: Version, Kind: "SpotRecovery"}
	StatefulSetGVK       = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	NodeProvisionGVK     = schema.GroupVersionKind{Group: "ml.dcn.ssu.ac.kr", Version: Version, Kind: "NodeProvision"}
	FluidMigrationGVK    = schema.GroupVersionKind{Group: "fluidcr.dcnlab.com", Version: Version, Kind: "FluidCRMigration"}
	PropagationPolicyGVK = schema.GroupVersionKind{
		Group:   "policy.karmada.io",
		Version: "v1alpha1",
		Kind:    "PropagationPolicy",
	}
)

type WorkloadRef struct {
	APIVersion string
	Kind       string
	Name       string
	UID        types.UID
}

type RuntimeSnapshot struct {
	Name               string
	Generation         int64
	WorkloadUID        types.UID
	StatusWorkloadUID  types.UID
	MemberWorkloadUID  types.UID
	SourceCluster      string
	Phase              string
	ExpectedWorldSize  int64
	Port               int64
	Container          string
	GlobalStep         int64
	CheckpointID       string
	ReadyRanks         int64
	WorldSize          int64
	ObservedGeneration int64
	Pods               []PodRuntime
	ObservedAt         string
}

type PodRuntime struct {
	Name               string
	UID                string
	Rank               int64
	GlobalStep         int64
	PreviousGlobalStep int64
	CheckpointID       string
	ObservedAt         string
	PreviousObservedAt string
}

type RiskSnapshot struct {
	Name                 string
	Generation           int64
	ObservedGeneration   int64
	LambdaPerHour        float64
	ObservedAt           string
	ValidUntil           string
	Ready                bool
	Source               string
	SpotPricePerHour     float64
	OnDemandPricePerHour float64
	Provider             string
	Region               string
	AvailabilityZone     string
	InstanceType         string
}

type CapacityDefaults struct {
	AWSCluster         string
	Region             string
	AvailabilityZone   string
	InstanceType       string
	AMI                string
	CredentialsName    string
	CredentialsNS      string
	SubnetID           string
	VPCID              string
	SecurityGroupIDs   []string
	IAMInstanceProfile string
	RootVolumeSizeGB   int64
	HardwareType       string
	NodeLabel          string
}

type CheckpointPolicy struct {
	MinIntervalSeconds int64
	MaxIntervalSeconds int64
	CandidateIntervals []int64
	RiskBands          []RiskBand
	MeasuredCosts      MeasuredCosts
	Resume             bool
}

type MeasuredCosts struct {
	CheckpointSeconds float64
	CopySeconds       float64
	ObservedAt        string
}

type RiskBand struct {
	MaxLambdaPerHour float64
	IntervalSeconds  int64
}

type PolicyInput struct {
	Namespace       string
	PolicyName      string
	PolicyUID       types.UID
	Generation      int64
	WorkloadRef     WorkloadRef
	RuntimeRefName  string
	RiskProfileName string
	SourceCluster   string
	TargetWorkers   int64
	MinOnDemand     int64
	Alpha           float64
	ForecastSeconds int64
	Capacity        CapacityDefaults
	Checkpoint      CheckpointPolicy
}

type Decision struct {
	DesiredWorkers            int64
	OnDemandWorkers           int64
	SpotWorkers               int64
	Alpha                     float64
	ForecastHorizonSeconds    int64
	LambdaPerHour             float64
	CheckpointIntervalSeconds int64
	CostEvaluated             bool
	IntervalCostEvaluated     bool
	ProvisioningBlocked       bool
	Reason                    string
}

func ReadPolicySpec(obj *unstructured.Unstructured) PolicyInput {
	resume, ok, _ := unstructured.NestedBool(obj.Object, "spec", "checkpoint", "resume")
	if !ok {
		resume = true
	}
	return PolicyInput{
		Namespace:       obj.GetNamespace(),
		PolicyName:      obj.GetName(),
		PolicyUID:       obj.GetUID(),
		Generation:      obj.GetGeneration(),
		WorkloadRef:     readWorkloadRef(obj),
		RuntimeRefName:  nestedStringDefault(obj.Object, obj.GetName()+"-runtime", "spec", "runtimeRef", "name"),
		RiskProfileName: nestedStringDefault(obj.Object, obj.GetName()+"-risk", "spec", "riskProfileRef", "name"),
		SourceCluster:   nestedStringDefault(obj.Object, "", "spec", "sourceCluster"),
		TargetWorkers:   nestedIntDefault(obj.Object, 0, "spec", "targetWorkers"),
		MinOnDemand:     nestedIntDefault(obj.Object, 0, "spec", "policy", "minOnDemand"),
		Alpha:           clampAlpha(nestedFloatDefault(obj.Object, DefaultAlpha, "spec", "policy", "alpha")),
		ForecastSeconds: maxInt64(1, nestedIntDefault(obj.Object, DefaultForecastHorizonSeconds, "spec", "policy", "forecastHorizonSeconds")),
		Capacity: CapacityDefaults{
			AWSCluster:         nestedStringDefault(obj.Object, DefaultAWSCluster, "spec", "capacity", "aws", "karmadaCluster"),
			Region:             nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "region"),
			AvailabilityZone:   nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "availabilityZone"),
			InstanceType:       nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "instanceType"),
			AMI:                nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "ami"),
			CredentialsName:    nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "credentialsRef", "name"),
			CredentialsNS:      nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "credentialsRef", "namespace"),
			SubnetID:           nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "subnetId"),
			VPCID:              nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "vpcId"),
			IAMInstanceProfile: nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "iamInstanceProfile"),
			RootVolumeSizeGB:   nestedIntDefault(obj.Object, 0, "spec", "capacity", "aws", "rootVolumeSizeGB"),
			HardwareType:       nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "hardwareType"),
			NodeLabel:          nestedStringDefault(obj.Object, "", "spec", "capacity", "aws", "nodeLabel"),
			SecurityGroupIDs:   nestedStringSlice(obj.Object, "spec", "capacity", "aws", "securityGroupIds"),
		},
		Checkpoint: CheckpointPolicy{
			MinIntervalSeconds: nestedIntDefault(obj.Object, 0, "spec", "checkpoint", "minIntervalSeconds"),
			MaxIntervalSeconds: nestedIntDefault(obj.Object, 0, "spec", "checkpoint", "maxIntervalSeconds"),
			CandidateIntervals: nestedIntSlice(obj.Object, "spec", "checkpoint", "candidateIntervalSeconds"),
			RiskBands:          nestedRiskBands(obj.Object, "spec", "checkpoint", "riskBands"),
			MeasuredCosts: MeasuredCosts{
				CheckpointSeconds: nestedFloatDefault(obj.Object, 0, "spec", "checkpoint", "measuredCosts", "checkpointSeconds"),
				CopySeconds:       nestedFloatDefault(obj.Object, 0, "spec", "checkpoint", "measuredCosts", "copySeconds"),
				ObservedAt:        nestedStringDefault(obj.Object, "", "spec", "checkpoint", "measuredCosts", "observedAt"),
			},
			Resume: resume,
		},
	}
}

func ReadRuntimeStatus(obj *unstructured.Unstructured, sourceCluster string) RuntimeSnapshot {
	selected := obj.Object
	for _, cluster := range nestedMapSlice(obj.Object, "status", "clusters") {
		if name, _, _ := unstructured.NestedString(cluster, "clusterName"); name == sourceCluster {
			if status, ok, _ := unstructured.NestedMap(cluster, "status"); ok {
				selected = map[string]interface{}{"status": status}
			} else {
				selected = cluster
			}
			break
		}
	}
	return RuntimeSnapshot{
		Name:               obj.GetName(),
		Generation:         obj.GetGeneration(),
		WorkloadUID:        types.UID(nestedStringDefault(obj.Object, "", "spec", "workloadRef", "uid")),
		StatusWorkloadUID:  types.UID(nestedStringDefault(selected, "", "status", "workloadUID")),
		MemberWorkloadUID:  types.UID(nestedStringDefault(selected, "", "status", "memberWorkloadUID")),
		SourceCluster:      nestedStringDefault(obj.Object, sourceCluster, "spec", "sourceCluster"),
		Phase:              nestedStringDefault(selected, "", "status", "phase"),
		ExpectedWorldSize:  nestedIntDefault(obj.Object, 0, "spec", "expectedWorldSize"),
		Port:               nestedIntDefault(obj.Object, DefaultControlPort, "spec", "port"),
		Container:          nestedStringDefault(obj.Object, "", "spec", "container"),
		GlobalStep:         nestedIntDefault(selected, 0, "status", "globalStep"),
		CheckpointID:       nestedStringDefault(selected, "", "status", "checkpointID"),
		ReadyRanks:         nestedIntDefault(selected, 0, "status", "readyRanks"),
		WorldSize:          nestedIntDefault(selected, 0, "status", "worldSize"),
		ObservedGeneration: nestedIntDefault(selected, 0, "status", "observedGeneration"),
		Pods:               readRuntimePods(selected),
		ObservedAt:         nestedStringDefault(selected, "", "status", "observedAt"),
	}
}

func ReadRiskStatus(obj *unstructured.Unstructured) RiskSnapshot {
	return RiskSnapshot{
		Name:                 obj.GetName(),
		Generation:           obj.GetGeneration(),
		ObservedGeneration:   nestedIntDefault(obj.Object, 0, "status", "observedGeneration"),
		LambdaPerHour:        nestedFloatDefault(obj.Object, nestedFloatDefault(obj.Object, -1, "spec", "staticLambdaPerHour"), "status", "lambdaPerHour"),
		ObservedAt:           nestedStringDefault(obj.Object, "", "status", "observedAt"),
		ValidUntil:           nestedStringDefault(obj.Object, "", "status", "validUntil"),
		Ready:                nestedBoolDefault(obj.Object, false, "status", "ready"),
		Source:               nestedStringDefault(obj.Object, "", "status", "source"),
		SpotPricePerHour:     nestedFloatDefault(obj.Object, 0, "status", "prices", "spotPricePerHour"),
		OnDemandPricePerHour: nestedFloatDefault(obj.Object, 0, "status", "prices", "onDemandPricePerHour"),
		Provider:             nestedStringDefault(obj.Object, "", "spec", "provider"),
		Region:               nestedStringDefault(obj.Object, "", "spec", "region"),
		AvailabilityZone:     nestedStringDefault(obj.Object, "", "spec", "availabilityZone"),
		InstanceType:         nestedStringDefault(obj.Object, "", "spec", "instanceType"),
	}
}

func Decide(input PolicyInput, runtime RuntimeSnapshot, risk RiskSnapshot) Decision {
	desired := input.TargetWorkers
	alpha := clampAlpha(input.Alpha)
	horizonSeconds := maxInt64(1, input.ForecastSeconds)
	riskKnown := risk.Ready && risk.LambdaPerHour >= 0
	if !riskKnown {
		return Decision{DesiredWorkers: desired, Alpha: alpha, ForecastHorizonSeconds: horizonSeconds, LambdaPerHour: risk.LambdaPerHour, CheckpointIntervalSeconds: applyIntervalBounds(conservativeInterval(input.Checkpoint), input.Checkpoint), CostEvaluated: false, ProvisioningBlocked: true, Reason: "risk_unavailable"}
	}
	spot := maxStableSpotWorkers(desired, risk.LambdaPerHour, float64(horizonSeconds)/3600, alpha)
	onDemand := desired - spot
	if onDemand < input.MinOnDemand {
		onDemand = input.MinOnDemand
		spot = desired - onDemand
	}
	interval, intervalEvaluated := AdaptiveCheckpointInterval(input.Checkpoint, risk, runtime, spot, time.Now().UTC())
	interval = applyIntervalBounds(interval, input.Checkpoint)
	return Decision{DesiredWorkers: desired, OnDemandWorkers: maxInt64(0, onDemand), SpotWorkers: maxInt64(0, spot), Alpha: alpha, ForecastHorizonSeconds: horizonSeconds, LambdaPerHour: risk.LambdaPerHour, CheckpointIntervalSeconds: interval, CostEvaluated: false, IntervalCostEvaluated: intervalEvaluated, Reason: "independent_spot_survival"}
}

func AdaptiveCheckpointInterval(checkpoint CheckpointPolicy, risk RiskSnapshot, runtime RuntimeSnapshot, spotWorkers int64, now time.Time) (int64, bool) {
	measured := checkpoint.MeasuredCosts
	observedAt, err := time.Parse(time.RFC3339, measured.ObservedAt)
	if err != nil || observedAt.After(now) || now.Sub(observedAt) > 10*time.Minute || measured.CheckpointSeconds <= 0 || measured.CopySeconds < 0 || !finite(measured.CheckpointSeconds) || !finite(measured.CopySeconds) || risk.LambdaPerHour < 0 || !finite(risk.LambdaPerHour) || spotWorkers <= 0 {
		return EstimateCheckpointIntervalSeconds(checkpoint, risk, runtime), false
	}
	lambdaJobPerSecond := risk.LambdaPerHour * float64(spotWorkers) / 3600
	candidates := checkpoint.CandidateIntervals
	if len(candidates) == 0 {
		candidates = []int64{30, 60, 120, 300, 600}
	}
	best := int64(0)
	bestObjective := math.Inf(1)
	for _, candidate := range candidates {
		if candidate <= 0 {
			continue
		}
		candidate = applyIntervalBounds(candidate, checkpoint)
		tau := float64(candidate)
		objective := measured.CheckpointSeconds/tau + lambdaJobPerSecond*tau/2 + math.Max(0, measured.CopySeconds/tau-1)
		if objective < bestObjective {
			bestObjective = objective
			best = candidate
		}
	}
	if best <= 0 {
		return EstimateCheckpointIntervalSeconds(checkpoint, risk, runtime), false
	}
	return best, true
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func RiskFreshForPolicy(input PolicyInput, risk RiskSnapshot, now time.Time) bool {
	if !risk.Ready || risk.ObservedGeneration != risk.Generation || risk.ObservedGeneration == 0 {
		return false
	}
	if risk.ValidUntil == "" {
		return false
	}
	validUntil, err := time.Parse(time.RFC3339, risk.ValidUntil)
	return err == nil && !now.After(validUntil)
}

func RuntimeReadyForCheckpoint(input PolicyInput, runtime RuntimeSnapshot, now time.Time) bool {
	if runtime.WorkloadUID != input.WorkloadRef.UID || runtime.WorkloadUID == "" {
		return false
	}
	if runtime.StatusWorkloadUID != input.WorkloadRef.UID || runtime.MemberWorkloadUID == "" {
		return false
	}
	if runtime.Phase != "Running" {
		return false
	}
	if runtime.ObservedGeneration != runtime.Generation || runtime.ObservedGeneration == 0 {
		return false
	}
	worldSize := firstPositive(runtime.WorldSize, runtime.ExpectedWorldSize, input.TargetWorkers)
	if worldSize == 0 || runtime.ReadyRanks != worldSize || int64(len(runtime.Pods)) != worldSize || worldSize != input.TargetWorkers {
		return false
	}
	observedAt, err := time.Parse(time.RFC3339, runtime.ObservedAt)
	if err != nil {
		return false
	}
	if observedAt.After(now) || now.Sub(observedAt) > 2*time.Minute {
		return false
	}
	ranks := map[int64]bool{}
	checkpointID := ""
	checkpointInitialized := false
	for _, pod := range runtime.Pods {
		if pod.Name == "" || pod.UID == "" || pod.Rank < 0 || pod.Rank >= worldSize || ranks[pod.Rank] {
			return false
		}
		ranks[pod.Rank] = true
		if !checkpointInitialized {
			checkpointID = pod.CheckpointID
			checkpointInitialized = true
		} else if pod.CheckpointID != checkpointID {
			return false
		}
		podObservedAt, err := time.Parse(time.RFC3339, pod.ObservedAt)
		if err != nil || podObservedAt.After(now) || now.Sub(podObservedAt) > 2*time.Minute {
			return false
		}
	}
	return true
}

func EstimateCheckpointIntervalSeconds(checkpoint CheckpointPolicy, risk RiskSnapshot, runtime RuntimeSnapshot) int64 {
	if runtime.WorldSize > 0 && runtime.ReadyRanks < runtime.WorldSize {
		return chooseCandidate(checkpoint, 60)
	}
	if !risk.Ready || risk.LambdaPerHour < 0 {
		return conservativeInterval(checkpoint)
	}
	for _, band := range riskBandsOrDefault(checkpoint) {
		if risk.LambdaPerHour <= band.MaxLambdaPerHour {
			return chooseCandidate(checkpoint, band.IntervalSeconds)
		}
	}
	return chooseCandidate(checkpoint, 30)
}

func NextCheckpointDue(now time.Time, lastStartedAt string, intervalSeconds int64) bool {
	if intervalSeconds <= 0 {
		intervalSeconds = DefaultCheckpointSeconds
	}
	if lastStartedAt == "" {
		return true
	}
	parsed, err := time.Parse(time.RFC3339, lastStartedAt)
	if err != nil {
		return true
	}
	return !now.Before(parsed.Add(time.Duration(intervalSeconds) * time.Second))
}

func maxStableSpotWorkers(total int64, lambdaPerHour float64, horizonHours float64, alpha float64) int64 {
	if total <= 0 {
		return 0
	}
	if lambdaPerHour == 0 || horizonHours <= 0 {
		return total
	}
	if lambdaPerHour < 0 {
		return 0
	}
	maxSpot := int64(math.Floor(-math.Log(alpha) / (lambdaPerHour * horizonHours)))
	if maxSpot > total {
		maxSpot = total
	}
	return maxInt64(0, maxSpot)
}

func readWorkloadRef(obj *unstructured.Unstructured) WorkloadRef {
	uid, _, _ := unstructured.NestedString(obj.Object, "spec", "workloadRef", "uid")
	return WorkloadRef{APIVersion: nestedStringDefault(obj.Object, "", "spec", "workloadRef", "apiVersion"), Kind: nestedStringDefault(obj.Object, "", "spec", "workloadRef", "kind"), Name: nestedStringDefault(obj.Object, "", "spec", "workloadRef", "name"), UID: types.UID(uid)}
}

func readRuntimePods(obj map[string]interface{}) []PodRuntime {
	items := nestedMapSlice(obj, "status", "pods")
	pods := make([]PodRuntime, 0, len(items))
	for _, item := range items {
		pods = append(pods, PodRuntime{Name: nestedStringDefault(item, "", "name"), UID: nestedStringDefault(item, "", "uid"), Rank: nestedIntDefault(item, 0, "rank"), GlobalStep: nestedIntDefault(item, 0, "globalStep"), PreviousGlobalStep: nestedIntDefault(item, 0, "previousGlobalStep"), CheckpointID: nestedStringDefault(item, "", "checkpointID"), ObservedAt: nestedStringDefault(item, "", "observedAt"), PreviousObservedAt: nestedStringDefault(item, "", "previousObservedAt")})
	}
	return pods
}

func nestedStringDefault(obj map[string]interface{}, def string, fields ...string) string {
	value, ok, _ := unstructured.NestedString(obj, fields...)
	if !ok || value == "" {
		return def
	}
	return value
}

func nestedBoolDefault(obj map[string]interface{}, def bool, fields ...string) bool {
	value, ok, _ := unstructured.NestedBool(obj, fields...)
	if !ok {
		return def
	}
	return value
}

func nestedIntDefault(obj map[string]interface{}, def int64, fields ...string) int64 {
	if value, ok, _ := unstructured.NestedInt64(obj, fields...); ok {
		return value
	}
	if value, ok, _ := unstructured.NestedFloat64(obj, fields...); ok {
		return int64(value)
	}
	if value, ok, _ := unstructured.NestedString(obj, fields...); ok {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return def
}

func nestedFloatDefault(obj map[string]interface{}, def float64, fields ...string) float64 {
	if value, ok, _ := unstructured.NestedFloat64(obj, fields...); ok {
		return value
	}
	if value, ok, _ := unstructured.NestedInt64(obj, fields...); ok {
		return float64(value)
	}
	if value, ok, _ := unstructured.NestedString(obj, fields...); ok {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return def
}

func nestedStringSlice(obj map[string]interface{}, fields ...string) []string {
	raw, ok, _ := unstructured.NestedStringSlice(obj, fields...)
	if ok {
		return raw
	}
	return nil
}

func nestedIntSlice(obj map[string]interface{}, fields ...string) []int64 {
	raw, ok, _ := unstructured.NestedSlice(obj, fields...)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(raw))
	for _, item := range raw {
		switch typed := item.(type) {
		case int64:
			out = append(out, typed)
		case int:
			out = append(out, int64(typed))
		case float64:
			out = append(out, int64(typed))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func nestedMapSlice(obj map[string]interface{}, fields ...string) []map[string]interface{} {
	raw, ok, _ := unstructured.NestedSlice(obj, fields...)
	if !ok {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		if typed, ok := item.(map[string]interface{}); ok {
			out = append(out, typed)
		}
	}
	return out
}

func nestedRiskBands(obj map[string]interface{}, fields ...string) []RiskBand {
	raw := nestedMapSlice(obj, fields...)
	bands := make([]RiskBand, 0, len(raw))
	for _, item := range raw {
		bands = append(bands, RiskBand{MaxLambdaPerHour: nestedFloatDefault(item, 0, "maxLambdaPerHour"), IntervalSeconds: nestedIntDefault(item, 0, "intervalSeconds")})
	}
	sort.Slice(bands, func(i, j int) bool { return bands[i].MaxLambdaPerHour < bands[j].MaxLambdaPerHour })
	return bands
}

func riskBandsOrDefault(checkpoint CheckpointPolicy) []RiskBand {
	if len(checkpoint.RiskBands) > 0 {
		return checkpoint.RiskBands
	}
	return []RiskBand{{MaxLambdaPerHour: 0.02, IntervalSeconds: 600}, {MaxLambdaPerHour: 0.05, IntervalSeconds: 300}, {MaxLambdaPerHour: 0.10, IntervalSeconds: 120}, {MaxLambdaPerHour: 0.20, IntervalSeconds: 60}}
}

func conservativeInterval(checkpoint CheckpointPolicy) int64 {
	return chooseCandidate(checkpoint, 60)
}

func chooseCandidate(checkpoint CheckpointPolicy, desired int64) int64 {
	candidates := checkpoint.CandidateIntervals
	if len(candidates) == 0 {
		candidates = []int64{30, 60, 120, 300, 600}
	}
	chosen := candidates[len(candidates)-1]
	for _, candidate := range candidates {
		if candidate >= desired {
			return candidate
		}
	}
	return chosen
}

func applyIntervalBounds(interval int64, checkpoint CheckpointPolicy) int64 {
	if checkpoint.MinIntervalSeconds > 0 && interval < checkpoint.MinIntervalSeconds {
		interval = checkpoint.MinIntervalSeconds
	}
	if checkpoint.MaxIntervalSeconds > 0 && interval > checkpoint.MaxIntervalSeconds {
		interval = checkpoint.MaxIntervalSeconds
	}
	return interval
}

func firstPositive(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func clampAlpha(value float64) float64 {
	if value <= 0 || value >= 1 {
		return DefaultAlpha
	}
	return value
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
