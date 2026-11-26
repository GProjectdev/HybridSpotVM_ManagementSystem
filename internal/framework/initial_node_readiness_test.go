package framework

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestInitialOnPremWaitsForEveryProvisionedNodeQualification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*corev1.Node)
		blocked bool
	}{
		{"qualified", func(*corev1.Node) {}, false},
		{"GPU-ready-before-TLS", func(n *corev1.Node) { delete(n.Labels, "migration.dcnlab.com/restore-from-file") }, true},
		{"missing-owner", func(n *corev1.Node) { delete(n.Labels, "ml.dcn.ssu.ac.kr/node-provision-uid") }, true},
		{"invalid-package", func(n *corev1.Node) { n.Annotations["migration.dcnlab.com/restore-package-sha256"] = "invalid" }, true},
		{"native-OnPrem", func(n *corev1.Node) { n.Labels = nil; n.Annotations = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "first", Labels: map[string]string{"ml.dcn.ssu.ac.kr/node-provision": "np", "ml.dcn.ssu.ac.kr/node-provision-uid": "np-uid", "migration.dcnlab.com/restore-from-file": "true"}, Annotations: map[string]string{"migration.dcnlab.com/restore-package-sha256": strings.Repeat("a", 64)}}}
			b := a.DeepCopy()
			b.Name = "second"
			tc.mutate(b)
			c := frameworkClient(a, b)
			members := func(string) (client.Reader, error) { return c, nil }
			o := WorkloadObservation{SelectedCluster: "onpre1", AllPodsScheduled: true, ScheduledNodes: map[string]string{"job-0": "first", "job-1": "second"}}
			err := initialOnPremInfrastructure(context.Background(), members, RunSpec{OnPremiseCluster: "onpre1"}, o)
			if (err != nil) != tc.blocked {
				t.Fatalf("blocked=%v: %v", tc.blocked, err)
			}
		})
	}
}
