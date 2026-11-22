package management

import (
	"context"
	"encoding/json"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func TestGroupEmergencySuccessorKeepsOriginalIntentAndCreatesRestore(t *testing.T) {
	now := mustParseTime(t, "2026-11-22T07:04:00Z")
	policy, old, objects := fixedEmergencyFixtures(now, true, spotInterruptionNotice)
	set := func(o *unstructured.Unstructured, value interface{}, fields ...string) {
		if e := unstructured.SetNestedField(o.Object, value, fields...); e != nil {
			t.Fatal(e)
		}
	}
	set(old, "first", "status", "spot", "eventID")
	set(old, "2026-11-22T07:03:51Z", "status", "spot", "noticeTime")
	f := checkpointReconcilerFixture(t, func() time.Time { return now }, objects...)
	r := &PolicyReconciler{Client: f.Client, APIReader: f.Client, Clock: func() time.Time { return now }}
	op := replacementOperationName(old.GetName(), string(old.GetUID()))
	replacement := replacementNodeProvisionName(old.GetName(), string(old.GetUID()))
	if e := r.recordGroupOperation(context.Background(), policy, old, op, replacement, "Spot", "first"); e != nil {
		t.Fatal(e)
	}
	current := p.NewObject("NodeRequest")
	if e := r.Get(context.Background(), client.ObjectKeyFromObject(policy), current); e != nil {
		t.Fatal(e)
	}
	raw := current.GetAnnotations()[groupOperationAnnotation]
	var intent groupOperation
	if e := json.Unmarshal([]byte(raw), &intent); e != nil {
		t.Fatal(e)
	}
	set(old, "successor", "status", "spot", "eventID")
	set(old, "2026-11-22T07:03:58Z", "status", "spot", "noticeTime")
	if e := r.Update(context.Background(), old); e != nil {
		t.Fatal(e)
	}
	if !matchesGroupEmergency(old, p.ReadPolicyInput(policy), intent) {
		t.Fatal("same-instance successor rejected")
	}
	if e := r.recordGroupOperation(context.Background(), policy, old, op, replacement, "Spot", "successor"); e != nil {
		t.Fatal(e)
	}
	if e := r.Get(context.Background(), client.ObjectKeyFromObject(policy), current); e != nil {
		t.Fatal(e)
	}
	if current.GetAnnotations()[groupOperationAnnotation] != raw {
		t.Fatal("immutable original intent overwritten")
	}
	np := p.NewObject("NodeProvision")
	np.SetNamespace(policy.GetNamespace())
	np.SetName(replacement)
	np.SetUID("new-uid")
	np.SetLabels(map[string]string{p.LabelPolicyUID: string(policy.GetUID())})
	np.SetAnnotations(map[string]string{groupOldUID: string(old.GetUID())})
	np.Object["spec"] = map[string]interface{}{"marketType": "Spot"}
	np.Object["status"] = map[string]interface{}{"phase": "Ready", "nodeName": "new-node"}
	if e := r.Create(context.Background(), np); e != nil {
		t.Fatal(e)
	}
	if _, e := r.reconcileGroupOperation(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}, groupStageRequest); e != nil {
		t.Fatal(e)
	}
	req := newRestoreRequest()
	if e := r.Get(context.Background(), client.ObjectKey{Namespace: policy.GetNamespace(), Name: op + "-group-restore"}, req); e != nil {
		t.Fatal(e)
	}
	if req.GetAnnotations()[annotationEmergencyEventID] != "first" {
		t.Fatal("lost original event provenance")
	}
	for _, tc := range []string{"provider", "uid", "earlier", "downgrade", "not-at-risk", "unbound"} {
		t.Run(tc, func(t *testing.T) {
			bad := old.DeepCopy()
			i := intent
			switch tc {
			case "provider":
				set(bad, "foreign", "status", "instanceId")
				set(bad, "foreign", "status", "spot", "instanceID")
			case "uid":
				bad.SetUID("foreign")
			case "earlier":
				set(bad, "2026-11-22T07:03:00Z", "status", "spot", "noticeTime")
			case "downgrade":
				set(bad, spotRebalanceRecommendation, "status", "spot", "signalType")
			case "not-at-risk":
				set(bad, false, "status", "spot", "atRisk")
			case "unbound":
				i.EmergencyBinding = nil
			}
			if matchesGroupEmergency(bad, p.ReadPolicyInput(policy), i) {
				t.Fatal("unsafe event admitted")
			}
		})
	}
}
