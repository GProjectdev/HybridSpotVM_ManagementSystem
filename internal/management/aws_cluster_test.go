package management

import (
	"context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestReplacementConfiguredAWSMember(t *testing.T) {
	for _, tc := range []struct {
		name, source, target, configured string
		accept                           bool
	}{
		{"legacy default", "aws", "aws", "", true},
		{"renamed member", "aws1", "aws1", "aws1", true},
		{"different configured member", "aws1", "aws1", "aws2", false},
		{"cross cluster rejected", "aws1", "onpre1", "aws1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := replacementOperationFixture()
			_ = unstructured.SetNestedField(op.Object, tc.source, "spec", "sourceCluster")
			_ = unstructured.SetNestedField(op.Object, tc.target, "spec", "targetCluster")
			spec, err := readReplacementSpec(op)
			policy := replacementPolicyFixture()
			if tc.configured != "" {
				_ = unstructured.SetNestedField(policy.Object, tc.configured, "spec", "capacity", "aws", "karmadaCluster")
			}
			r := replacementReconcilerFixture(t, mustParseTime(t, "2026-09-26T00:00:00Z"), policy)
			if err == nil {
				err = r.verifyReplacementPolicy(context.Background(), "default", spec)
			}
			if (err == nil) != tc.accept {
				t.Fatalf("accept=%v error=%v", tc.accept, err)
			}
		})
	}
}

func TestRecoveryConfiguredAWSMember(t *testing.T) {
	policy := replacementPolicyFixture()
	_ = unstructured.SetNestedField(policy.Object, "aws1", "spec", "capacity", "aws", "karmadaCluster")
	rr := replacementReconcilerFixture(t, mustParseTime(t, "2026-09-26T00:00:00Z"), policy)
	r := &RecoveryReconciler{Client: rr.Client, APIReader: rr.APIReader}
	spec := recoverySpec{PolicyName: "policy", PolicyUID: "policy-uid", PolicyGeneration: 3, WorkloadUID: "workload-uid", SourceCluster: "aws1"}
	if err := r.verifyPolicy(context.Background(), "default", spec); err != nil {
		t.Fatal(err)
	}
	spec.SourceCluster = "onpre1"
	if err := r.verifyPolicy(context.Background(), "default", spec); err == nil {
		t.Fatal("unconfigured source accepted")
	}
}
