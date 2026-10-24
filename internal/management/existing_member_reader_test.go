package management

import (
	"context"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
)

func TestExistingMemberReaderHasNoAmbientFallback(t *testing.T) {
	for _, tc := range []struct{ directory, cluster string }{
		{"", "aws"}, {"relative", "aws"}, {t.TempDir(), "../aws"}, {t.TempDir(), "aws"},
	} {
		input := p.PolicyInput{Capacity: p.CapacityDefaults{AWSCluster: tc.cluster}}
		resolver := newExistingNodeResolver(nil, tc.directory)
		if _, err := resolver(context.Background(), input); err == nil {
			t.Fatalf("accepted missing/untrusted member configuration: %+v", tc)
		}
	}
}
