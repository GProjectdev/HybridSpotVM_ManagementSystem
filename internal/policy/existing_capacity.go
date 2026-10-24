package policy

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	CapacityModeProvisioned  = "Provisioned"
	CapacityModeExisting     = "Existing"
	MaxExistingCapacityNodes = 128
)

type ExistingCapacityNode struct {
	Rank     int64
	NodeName string
	NodeUID  types.UID
}

// Preserve malformed opt-ins as validation errors instead of silently provisioning.
func readExistingCapacity(obj *unstructured.Unstructured, capacity *CapacityDefaults) {
	capacity.Mode = CapacityModeProvisioned
	raw, found, err := unstructured.NestedFieldNoCopy(obj.Object, "spec", "capacity")
	if err != nil {
		capacity.readError = err
		return
	}
	if !found {
		return
	}
	fields, ok := raw.(map[string]interface{})
	if !ok {
		capacity.readError = fmt.Errorf("spec.capacity must be an object")
		return
	}
	if rawMode, exists := fields["mode"]; exists {
		mode, ok := rawMode.(string)
		if !ok || mode == "" {
			capacity.readError = fmt.Errorf("spec.capacity.mode must be Provisioned or Existing")
			return
		}
		capacity.Mode = mode
	}
	if capacity.Mode == CapacityModeExisting {
		if cluster, found, err := unstructured.NestedString(obj.Object, "spec", "capacity", "aws", "karmadaCluster"); err != nil || (found && cluster == "") {
			capacity.readError = fmt.Errorf("spec.capacity.aws.karmadaCluster must be a nonempty string")
			return
		}
	}
	rawNodes, exists := fields["existingNodes"]
	if !exists {
		return
	}
	if capacity.Mode != CapacityModeExisting {
		capacity.readError = fmt.Errorf("existingNodes requires capacity.mode Existing")
		return
	}
	nodes, ok := rawNodes.([]interface{})
	if !ok {
		capacity.readError = fmt.Errorf("spec.capacity.existingNodes must be an array")
		return
	}
	for _, rawNode := range nodes {
		node, ok := rawNode.(map[string]interface{})
		if !ok {
			capacity.readError = fmt.Errorf("existingNodes entries must be objects")
			return
		}
		rank, rankOK := node["rank"].(int64)
		name, nameOK := node["nodeName"].(string)
		uid, uidOK := node["nodeUID"].(string)
		if !rankOK || !nameOK || !uidOK || len(node) != 3 {
			capacity.readError = fmt.Errorf("existingNodes entries require only integer rank, string nodeName and string nodeUID")
			return
		}
		capacity.ExistingNodes = append(capacity.ExistingNodes, ExistingCapacityNode{Rank: rank, NodeName: name, NodeUID: types.UID(uid)})
	}
}

func ValidateCapacity(input PolicyInput) error {
	c := input.Capacity
	if c.readError != nil {
		return fmt.Errorf("invalid capacity: %w", c.readError)
	}
	switch c.Mode {
	case "", CapacityModeProvisioned:
		if len(c.ExistingNodes) != 0 {
			return fmt.Errorf("existingNodes requires capacity.mode Existing")
		}
		return nil
	case CapacityModeExisting:
	default:
		return fmt.Errorf("spec.capacity.mode must be Provisioned or Existing")
	}
	if input.TargetWorkers < 1 || input.TargetWorkers > MaxExistingCapacityNodes || int64(len(c.ExistingNodes)) != input.TargetWorkers {
		return fmt.Errorf("Existing capacity requires one node per rank for 1..%d targetWorkers", MaxExistingCapacityNodes)
	}
	if len(validation.IsDNS1123Subdomain(c.AWSCluster)) != 0 {
		return fmt.Errorf("Existing capacity requires a valid target cluster")
	}
	if input.FixedOnDemand != nil && *input.FixedOnDemand != input.TargetWorkers {
		return fmt.Errorf("Existing capacity cannot request a Spot worker composition")
	}
	seenRanks, seenNames, seenUIDs := map[int64]bool{}, map[string]bool{}, map[types.UID]bool{}
	for _, node := range c.ExistingNodes {
		if node.Rank < 0 || node.Rank >= input.TargetWorkers || seenRanks[node.Rank] {
			return fmt.Errorf("existingNodes ranks must cover [0,targetWorkers) exactly once")
		}
		if len(validation.IsDNS1123Subdomain(node.NodeName)) != 0 || seenNames[node.NodeName] {
			return fmt.Errorf("existingNodes nodeName must be valid and unique")
		}
		uid := string(node.NodeUID)
		if uid == "" || len(uid) > 128 || strings.ContainsAny(uid, " \t\r\n") || seenUIDs[node.NodeUID] {
			return fmt.Errorf("existingNodes nodeUID must be nonempty, whitespace-free and unique")
		}
		seenRanks[node.Rank], seenNames[node.NodeName], seenUIDs[node.NodeUID] = true, true, true
	}
	return nil
}
