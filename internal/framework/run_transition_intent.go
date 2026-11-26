package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func activeAllocationNames(run, cap *unstructured.Unstructured) []string {
	if names, ok, _ := unstructured.NestedStringSlice(run.Object, "status", "activeAllocations"); ok {
		return names
	}
	var cs CapacitySpec
	if cap == nil || decodeSpec(cap, &cs) != nil {
		return nil
	}
	var out []string
	for _, a := range cs.Allocations {
		if a.State != "Retire" && a.State != "Fence" {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}
func activeCapacityView(run, cap *unstructured.Unstructured) *unstructured.Unstructured {
	if cap == nil {
		return nil
	}
	names := activeAllocationNames(run, cap)
	var cs CapacitySpec
	if decodeSpec(cap, &cs) != nil {
		return cap
	}
	allocations := []Allocation{}
	for _, name := range names {
		for _, a := range cs.Allocations {
			if a.Name == name {
				allocations = append(allocations, a)
			}
		}
	}
	cs.Allocations = allocations
	view := cap.DeepCopy()
	view.Object["spec"] = encode(cs)
	return view
}
func capturedSources(run, work, cap *unstructured.Unstructured, rs RunSpec) ([]SourcePod, error) {
	permit, found, _ := unstructured.NestedMap(run.Object, "status", "executionPermit")
	if !found {
		return nil, fmt.Errorf("source execution permit missing")
	}
	pods, _, _ := unstructured.NestedStringMap(permit, "pods")
	nodes, _, _ := unstructured.NestedStringMap(permit, "nodes")
	if int64(len(pods)) != rs.WorldSize {
		return nil, fmt.Errorf("incomplete admitted source world")
	}
	entries := map[string]map[string]interface{}{}
	if cap != nil {
		all, _, _ := unstructured.NestedMap(cap.Object, "status", "allocations")
		for name, raw := range all {
			m := raw.(map[string]interface{})
			if n, _ := m["nodeName"].(string); n != "" {
				m["allocationName"] = name
				entries[n] = m
			}
		}
	}
	out := []SourcePod{}
	for rank := int64(0); rank < rs.WorldSize; rank++ {
		name := fmt.Sprintf("%s-%d", work.GetName(), rank)
		src := SourcePod{Rank: rank, PodName: name, PodUID: pods[name], NodeName: nodes[name]}
		if src.PodUID == "" || src.NodeName == "" {
			return nil, fmt.Errorf("source Pod/Node identity missing")
		}
		if str(run, "status", "committedCluster") == rs.Cloud.Cluster {
			m := &unstructured.Unstructured{Object: entries[src.NodeName]}
			src.NodeProvisionRef = &ProviderRef{Name: str(m, "allocationName"), UID: str(m, "memberUID"), InstanceID: str(m, "instanceID")}
			if src.NodeProvisionRef.Name == "" || src.NodeProvisionRef.UID == "" || src.NodeProvisionRef.InstanceID == "" {
				return nil, fmt.Errorf("source allocation receipt missing")
			}
		}
		out = append(out, src)
	}
	return out, nil
}

// transitionTrigger returns an immutable recovery/replacement intent. Risk alone
// does not prove an interruption, and a temporary telemetry gap is not Failover.
func (r *RunReconciler) transitionTrigger(ctx context.Context, run, work, cap *unstructured.Unstructured, rs RunSpec, o WorkloadObservation, status map[string]interface{}) (bool, error) {
	if !rs.Cloud.Allowed || !rs.ExecutionGate || rs.Suspend || str(run, "status", "committedCluster") == "" || executionID(run) == "" {
		return false, nil
	}
	source := str(run, "status", "committedCluster")
	mode, trigger := "", ""
	deadline := ""
	replace := map[string]string{}
	var cs CapacitySpec
	if cap != nil {
		if decodeSpec(cap, &cs) != nil {
			return false, fmt.Errorf("invalid current capacity")
		}
	}
	names := activeAllocationNames(run, cap)
	markets := map[string]string{}
	for _, a := range cs.Allocations {
		markets[a.Name] = a.Market
	}
	if source == rs.Cloud.Cluster {
		if cap == nil || integer(cap, "status", "observedGeneration") != cap.GetGeneration() {
			return false, nil
		}
		for _, name := range names {
			entry := &unstructured.Unstructured{}
			entry.Object, _, _ = unstructured.NestedMap(cap.Object, "status", "allocations", name)
			if markets[name] != "spot" || !boolAt(entry, "spot", "atRisk") || str(entry, "spot", "eventID") == "" || str(entry, "instanceID") == "" || str(entry, "spot", "instanceID") != str(entry, "instanceID") {
				continue
			}
			signal := str(entry, "spot", "signalType")
			if signal != "InterruptionNotice" {
				continue
			}
			at, e := time.Parse(time.RFC3339Nano, str(entry, "spot", "noticeTime"))
			if e != nil || at.After(r.now()) {
				continue
			}
			end, e := time.Parse(time.RFC3339Nano, str(entry, "spot", "interruptionTime"))
			if e != nil {
				return false, fmt.Errorf("interruption timestamp required")
			}
			end = end.Add(-10 * time.Second)
			if end.After(r.now().Add(110 * time.Second)) {
				end = r.now().Add(110 * time.Second)
			}
			mode = "Preemption"
			trigger = str(entry, "spot", "eventID")
			deadline = stamp(end)
			replace[name] = markets[name]
			break
		}
	}
	if source == rs.Cloud.Cluster && mode == "" {
		reader, e := r.Members(source)
		if e != nil {
			return false, e
		}
		for _, name := range names {
			nodeName := str(cap, "status", "allocations", name, "nodeName")
			if nodeName == "" {
				continue
			}
			node := &corev1.Node{}
			err := reader.Get(ctx, client.ObjectKey{Name: nodeName}, node)
			lost := apierrors.IsNotFound(err)
			if err != nil && !lost {
				return false, err
			}
			if err == nil {
				if string(node.UID) != str(cap, "status", "allocations", name, "nodeUID") {
					return false, fmt.Errorf("source Node incarnation changed")
				}
				for _, condition := range node.Status.Conditions {
					if condition.Type == corev1.NodeReady && condition.Status != corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() && r.now().Sub(condition.LastTransitionTime.Time) >= 60*time.Second {
						lost = true
					}
				}
			}
			if lost {
				mode = "Preemption"
				trigger = "unannounced-node-loss/" + str(cap, "status", "allocations", name, "memberUID")
				deadline = stamp(r.now())
				replace[name] = markets[name]
				break
			}
		}
	}
	if source == rs.OnPremiseCluster && rs.Failover.Enabled {
		failed, reason, e := r.confirmedSourceFailure(ctx, source, run, work, rs)
		if e != nil {
			return false, e
		}
		if failed {
			mode = "Failover"
			trigger = reason
			deadline = stamp(r.now())
		}
	}
	if mode == "" && source == rs.Cloud.Cluster && o.Running {
		d, receipt, e := r.operatingCloudDecision(ctx, run, work, cap, rs, o)
		if e != nil {
			status["resourcePolicyObservation"] = map[string]interface{}{"ready": false, "reason": e.Error(), "observedAt": stamp(r.now())}
			return false, nil
		}
		status["resourcePolicyObservation"] = receipt
		currentOD := int64(0)
		for _, name := range names {
			if markets[name] == "on-demand" {
				currentOD++
			}
		}
		targetMarket := "on-demand"
		remaining := d.OnDemandWorkers - currentOD
		if remaining < 0 {
			targetMarket = "spot"
			remaining = -remaining
		}
		slots := rs.Cloud.MaxWorkers - rs.WorldSize
		for _, name := range names {
			if remaining == 0 || int64(len(replace)) >= slots {
				break
			}
			if markets[name] != targetMarket {
				if targetMarket == "spot" && allocationHeld(run, name, r.now()) {
					continue
				}
				replace[name] = targetMarket
				remaining--
			}
		}
		if len(replace) > 0 {
			mode = "PlannedReplacement"
			body, _ := json.Marshal(receipt)
			sum := sha256.Sum256(body)
			trigger = "risk-" + hex.EncodeToString(sum[:8])
		}
	}
	if mode == "" {
		return false, nil
	}
	if mode == "Failover" && cap == nil {
		var e error
		cap, e = r.initialCapacity(ctx, run, rs, status, true)
		if e != nil {
			return false, e
		}
		_ = decodeSpec(cap, &cs)
		names = activeAllocationNames(run, cap)
	}
	sources, e := capturedSources(run, work, cap, rs)
	if e != nil {
		return false, e
	}
	if mode == "Preemption" && rs.Cloud.MaxWorkers < rs.WorldSize+1 {
		return false, fmt.Errorf("replacement requires approved temporary capacity headroom")
	}
	sum := sha256.Sum256([]byte(string(run.GetUID()) + "/" + executionID(run) + "/" + mode + "/" + trigger))
	op := "transition-" + hex.EncodeToString(sum[:12])
	ts := TransitionSpec{WorkloadTemplateSHA256: workloadTemplateSHA(work), RunRef: ObjectRef{Name: run.GetName(), UID: string(run.GetUID())}, Mode: mode, WorkloadUID: string(work.GetUID()), ExecutionID: executionID(run), WorldSize: rs.WorldSize, SourceCluster: source, TargetCluster: rs.Cloud.Cluster, CapacityRef: ObjectRef{Name: cap.GetName(), UID: string(cap.GetUID())}, SourcePods: sources, EmergencyDeadline: deadline, TriggerID: trigger}
	if mode == "Failover" {
		ts.TargetAllocations = names
	} else {
		// Preserve rank-to-node placement for unchanged workers; every changed worker
		// gets a new immutable allocation name and its original is retained.
		for rank, src := range sources {
			old := src.NodeProvisionRef.Name
			newMarket, change := replace[old]
			if !change {
				ts.TargetAllocations = append(ts.TargetAllocations, old)
				continue
			}
			name := fmt.Sprintf("%s-%d", op, rank)
			ts.AddedAllocations = append(ts.AddedAllocations, Allocation{Name: name, Market: newMarket, State: "Prepare"})
			ts.TargetAllocations = append(ts.TargetAllocations, name)
			ts.RetireAllocations = append(ts.RetireAllocations, old)
		}
	}
	t := named("TrainingTransition", run.GetNamespace(), op)
	setOwner(t, run)
	t.SetLabels(map[string]string{RunUIDLabel: string(run.GetUID())})
	t.Object["spec"] = encode(ts)
	if e = ts.validate(run, work, t, rs); e != nil {
		return false, e
	}
	if e = r.Create(ctx, t); e != nil {
		return false, e
	}
	status["activeTransition"] = map[string]interface{}{"name": t.GetName(), "uid": string(t.GetUID()), "mode": mode, "triggerID": trigger}
	return true, nil
}
func allocationHeld(run *unstructured.Unstructured, name string, now time.Time) bool {
	history, _, _ := unstructured.NestedMap(run.Object, "status", "verifiedTransitions")
	for _, raw := range history {
		v, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, fmt.Sprint(v["verifiedAt"]))
		if e != nil || now.Before(at) || !now.Before(at.Add(time.Hour)) {
			continue
		}
		names, _, _ := unstructured.NestedStringSlice(v, "targetAllocations")
		for _, n := range names {
			if n == name {
				return true
			}
		}
	}
	return false
}
func (r *RunReconciler) confirmedSourceFailure(ctx context.Context, cluster string, run, work *unstructured.Unstructured, rs RunSpec) (bool, string, error) {
	if !rs.Failover.Enabled {
		return false, "", nil
	}
	grace, restartThreshold := rs.Failover.limits()
	c := object("cluster.karmada.io/v1alpha1", "Cluster")
	if e := r.Get(ctx, client.ObjectKey{Name: cluster}, c); e != nil {
		return false, "", e
	}
	conds, _, _ := unstructured.NestedSlice(c.Object, "status", "conditions")
	for _, raw := range conds {
		m := &unstructured.Unstructured{Object: raw.(map[string]interface{})}
		at, e := time.Parse(time.RFC3339Nano, str(m, "lastTransitionTime"))
		if str(m, "type") == "Ready" && str(m, "status") != "True" && e == nil && !at.After(r.now()) && r.now().Sub(at) >= grace {
			return true, "cluster-not-ready/" + string(c.GetUID()) + "/" + stamp(at), nil
		}
	}
	reader, e := r.Members(cluster)
	if e != nil {
		return false, "", e
	}
	pods, _, _ := unstructured.NestedStringMap(run.Object, "status", "executionPermit", "pods")
	for name, uid := range pods {
		pod := &corev1.Pod{}
		e = reader.Get(ctx, key(run.GetNamespace(), name), pod)
		if e != nil {
			return false, "", e
		}
		if string(pod.UID) != uid {
			return false, "", fmt.Errorf("source Pod incarnation changed without transition")
		}
		if pod.Status.Phase == corev1.PodFailed {
			return true, "pod-failed/" + uid, nil
		}
		// Waiting on image pulls, readiness, or checkpoint pause is not a crash.
		for _, state := range pod.Status.ContainerStatuses {
			if rs.Container != "" && state.Name != rs.Container {
				continue
			}
			if int64(state.RestartCount) < restartThreshold {
				continue
			}
			// During restart backoff some kubelet/CRI combinations retain the
			// current Terminated state instead of publishing Waiting. Both carry
			// direct failure evidence; an old failure while Running is insufficient.
			last := state.State.Terminated
			if last == nil && state.State.Waiting != nil && state.State.Waiting.Reason == "CrashLoopBackOff" {
				last = state.LastTerminationState.Terminated
			}
			if last == nil || last.ExitCode == 0 || last.ExitCode == 99 || last.FinishedAt.IsZero() || last.FinishedAt.After(r.now()) {
				continue
			}
			if !pod.CreationTimestamp.IsZero() && r.now().Sub(pod.CreationTimestamp.Time) >= grace {
				return true, "application-crash/" + uid, nil
			}
		}
		node := &corev1.Node{}
		if pod.Spec.NodeName != "" {
			if e = reader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); e != nil {
				return false, "", e
			}
			for _, condition := range node.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status != corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() && !condition.LastTransitionTime.After(r.now()) && r.now().Sub(condition.LastTransitionTime.Time) >= grace {
					return true, "node-not-ready/" + string(node.UID) + "/" + stamp(condition.LastTransitionTime.Time), nil
				}
			}
		}
	}
	return false, "", nil
}
func (r *RunReconciler) operatingCloudDecision(ctx context.Context, run, work, cap *unstructured.Unstructured, s RunSpec, o WorkloadObservation) (p.Decision, map[string]interface{}, error) {
	d, receipt, e := initialCloudDecision(ctx, r.Client, run, s, r.now())
	if e != nil {
		return d, nil, e
	}
	template := object("ml.dcn.ssu.ac.kr/v1alpha1", "NodeProvision")
	var templateSpec map[string]interface{}
	if e = json.Unmarshal([]byte(str(cap, "status", "templateJSON")), &templateSpec); e != nil {
		return d, nil, e
	}
	template.Object["spec"] = templateSpec
	in, e := resourcePolicyInput(run, template, s)
	if e != nil {
		return d, nil, e
	}
	in.SourceCluster = o.SelectedCluster
	in.WorkloadRef = p.WorkloadRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: work.GetName(), UID: work.GetUID()}
	in.RuntimeRefName = run.GetName() + "-runtime"
	in.Economics = p.EconomicsPolicy{Enabled: true, LossCostPerEviction: -1, OnDemandDurationSeconds: 3600}
	riskObj := named("SpotRiskProfile", run.GetNamespace(), s.Cloud.RiskProfileRef)
	if e = r.Get(ctx, client.ObjectKeyFromObject(riskObj), riskObj); e != nil {
		return d, nil, e
	}
	risk := p.ReadRiskStatus(riskObj)
	if !p.RiskFreshForPolicy(in, risk, r.now()) {
		return d, nil, fmt.Errorf("risk changed or expired during policy evaluation")
	}
	schedule := p.NewObject("FluidCRMigration")
	if e = r.Get(ctx, key(run.GetNamespace(), run.GetName()+"-periodic"), schedule); e == nil {
		reader, err := r.Members(o.SelectedCluster)
		if err != nil {
			return d, nil, err
		}
		costs, measured, reason := latestCheckpointCosts(ctx, reader, schedule, run, work, o, r.now(), p.CheckpointCostMaxAge(in.Checkpoint))
		receipt["checkpointCostSource"] = reason
		if measured {
			in.Checkpoint.MeasuredCosts = costs
			receipt["checkpointCosts"] = encode(costs)
		}
	}
	if e = management.ApplyVerifiedRecoveryEconomics(ctx, r.Client, &in, o.Runtime, risk, r.now()); e != nil {
		return d, nil, e
	}
	d = p.DecideAt(in, o.Runtime, risk, r.now())
	receipt["decision"] = encode(d)
	receipt["observedAt"] = stamp(r.now())
	return d, receipt, nil
}
