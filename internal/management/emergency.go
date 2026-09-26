package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	annotationEmergencyEventID    = "training.dcnlab.com/emergency-event-id"
	annotationEmergencyInstanceID = "training.dcnlab.com/emergency-instance-id"
	annotationEmergencyNodeName   = "training.dcnlab.com/emergency-node-name"
	annotationEmergencyNodeUID    = "training.dcnlab.com/emergency-node-uid"
)

type emergencySpotEvent struct {
	EventID    string
	InstanceID string
	NodeName   string
	NodeUID    string
}

func (r *CheckpointReconciler) emergencySpotEvent(ctx context.Context, input trainingpolicy.PolicyInput) (*emergencySpotEvent, error) {
	event, _, err := r.unhandledEmergencySpotEventWithHandled(ctx, input)
	return event, err
}

func (r *CheckpointReconciler) unhandledEmergencySpotEvent(ctx context.Context, input trainingpolicy.PolicyInput) (*emergencySpotEvent, error) {
	event, _, err := r.unhandledEmergencySpotEventWithHandled(ctx, input)
	return event, err
}

func (r *CheckpointReconciler) unhandledEmergencySpotEventWithHandled(ctx context.Context, input trainingpolicy.PolicyInput) (*emergencySpotEvent, map[string]bool, error) {
	list := trainingpolicy.NewList("NodeProvision")
	labels := client.MatchingLabels{trainingpolicy.LabelPolicyUID: string(input.PolicyUID)}
	if err := r.List(ctx, list, client.InNamespace(input.Namespace), labels); err != nil {
		return nil, nil, fmt.Errorf("list NodeProvision emergency signals: %w", err)
	}
	events := make([]emergencySpotEvent, 0)
	for i := range list.Items {
		if event, ok := readEmergencySpotEvent(&list.Items[i], input); ok {
			events = append(events, event)
		}
	}
	if len(events) == 0 {
		return nil, nil, nil
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].EventID != events[j].EventID {
			return events[i].EventID < events[j].EventID
		}
		if events[i].InstanceID != events[j].InstanceID {
			return events[i].InstanceID < events[j].InstanceID
		}
		return events[i].NodeName < events[j].NodeName
	})
	handled, err := r.handledEmergencyEventKeys(ctx, input)
	if err != nil {
		return nil, nil, err
	}
	for i := range events {
		if !handled[events[i].Key()] {
			return &events[i], handled, nil
		}
	}
	return nil, handled, nil
}

func readEmergencySpotEvent(obj *unstructured.Unstructured, input trainingpolicy.PolicyInput) (emergencySpotEvent, bool) {
	if obj.GetLabels()[trainingpolicy.LabelPolicyUID] != string(input.PolicyUID) {
		return emergencySpotEvent{}, false
	}
	observedCluster, _, _ := unstructured.NestedString(obj.Object, "status", "observedCluster")
	if observedCluster != input.SourceCluster && observedCluster != input.Capacity.AWSCluster {
		return emergencySpotEvent{}, false
	}
	instanceID, _, _ := unstructured.NestedString(obj.Object, "status", "instanceId")
	spot, ok, _ := unstructured.NestedMap(obj.Object, "status", "spot")
	if !ok || instanceID == "" {
		return emergencySpotEvent{}, false
	}
	atRisk, _, _ := unstructured.NestedBool(spot, "atRisk")
	eventID, _, _ := unstructured.NestedString(spot, "eventID")
	spotInstanceID, _, _ := unstructured.NestedString(spot, "instanceID")
	if !atRisk || eventID == "" || spotInstanceID != instanceID {
		return emergencySpotEvent{}, false
	}
	return emergencySpotEvent{EventID: eventID, InstanceID: instanceID, NodeName: obj.GetName(), NodeUID: string(obj.GetUID())}, true
}

func (r *CheckpointReconciler) emergencyCheckpoint(ctx context.Context, input trainingpolicy.PolicyInput, event emergencySpotEvent) (*unstructured.Unstructured, error) {
	list, err := r.emergencyCheckpoints(ctx, input)
	if err != nil {
		return nil, err
	}
	key := event.Key()
	for i := range list.Items {
		item := &list.Items[i]
		if emergencyEventKey(item.GetAnnotations()) == key {
			return item, nil
		}
	}
	return nil, nil
}

func (r *CheckpointReconciler) handledEmergencyEventKeys(ctx context.Context, input trainingpolicy.PolicyInput) (map[string]bool, error) {
	list, err := r.emergencyCheckpoints(ctx, input)
	if err != nil {
		return nil, err
	}
	handled := map[string]bool{}
	for i := range list.Items {
		if key := emergencyEventKey(list.Items[i].GetAnnotations()); key != "" {
			handled[key] = true
		}
	}
	return handled, nil
}

func (r *CheckpointReconciler) emergencyCheckpoints(ctx context.Context, input trainingpolicy.PolicyInput) (*unstructured.UnstructuredList, error) {
	list := trainingpolicy.NewList("FluidCRMigration")
	labels := client.MatchingLabels{trainingpolicy.LabelPolicyUID: string(input.PolicyUID), trainingpolicy.LabelRole: "checkpoint"}
	if err := r.List(ctx, list, client.InNamespace(input.Namespace), labels); err != nil {
		return nil, fmt.Errorf("list FluidCRMigration emergency checkpoints: %w", err)
	}
	return list, nil
}

func newEmergencyCheckpoint(input trainingpolicy.PolicyInput, runtime trainingpolicy.RuntimeSnapshot, event emergencySpotEvent, now time.Time, intervalSeconds int64) *unstructured.Unstructured {
	migration := trainingpolicy.NewFluidCRMigration(input, runtime, now, intervalSeconds)
	name := fmt.Sprintf("%s-emergency-%s", dnsLabelPrefix(input.PolicyName, 35), eventHash(string(input.PolicyUID), event.Key()))
	migration.SetName(name)
	annotations := migration.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["training.dcnlab.com/checkpoint-id"] = name
	annotations[annotationEmergencyEventID] = event.EventID
	annotations[annotationEmergencyInstanceID] = event.InstanceID
	annotations[annotationEmergencyNodeName] = event.NodeName
	annotations[annotationEmergencyNodeUID] = event.NodeUID
	migration.SetAnnotations(annotations)
	return migration
}

func (e emergencySpotEvent) Key() string {
	if e.NodeUID != "" {
		return e.EventID + "|" + e.InstanceID + "|" + e.NodeUID
	}
	return e.EventID + "|" + e.InstanceID
}

func emergencyEventKey(annotations map[string]string) string {
	if annotations == nil || annotations[annotationEmergencyEventID] == "" || annotations[annotationEmergencyInstanceID] == "" {
		return ""
	}
	if annotations[annotationEmergencyNodeUID] != "" {
		return annotations[annotationEmergencyEventID] + "|" + annotations[annotationEmergencyInstanceID] + "|" + annotations[annotationEmergencyNodeUID]
	}
	return annotations[annotationEmergencyEventID] + "|" + annotations[annotationEmergencyInstanceID]
}

func eventHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])[:16]
}

func dnsLabelPrefix(value string, max int) string {
	value = strings.Trim(value, "-.")
	if len(value) <= max {
		return value
	}
	return strings.Trim(value[:max], "-.")
}
