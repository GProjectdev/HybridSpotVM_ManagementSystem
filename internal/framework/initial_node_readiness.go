package framework

import (
	"context"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A provisioned OnPrem worker can advertise its GPU before its kubelet TLS and
// restore runtime checks finish. Do not release application execution in that
// interval. Native, administrator-managed OnPrem nodes keep their existing path.
func initialOnPremInfrastructure(ctx context.Context, members MemberReader, s RunSpec, o WorkloadObservation) error {
	if !o.AllPodsScheduled || o.SelectedCluster != s.OnPremiseCluster {
		return nil
	}
	reader, err := members(o.SelectedCluster)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, name := range o.ScheduledNodes {
		if seen[name] {
			continue
		}
		seen[name] = true
		node := &corev1.Node{}
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
			return fmt.Errorf("initial source Node observation: %w", err)
		}
		if node.Labels["ml.dcn.ssu.ac.kr/node-provision"] == "" {
			continue
		}
		digest, err := hex.DecodeString(node.Annotations["migration.dcnlab.com/restore-package-sha256"])
		if node.Labels["ml.dcn.ssu.ac.kr/node-provision-uid"] == "" || node.Labels["migration.dcnlab.com/restore-from-file"] != "true" || err != nil || len(digest) != 32 {
			return fmt.Errorf("provisioned OnPrem node %s has not completed authenticated Checkpoint/Restore runtime qualification", name)
		}
	}
	return nil
}
