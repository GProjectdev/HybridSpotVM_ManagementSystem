package manifests_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestManagementRBACRespectsUserOwnedIntent(t *testing.T) {
	data, err := os.ReadFile("../../config/karmada/access.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var role rbacv1.ClusterRole
	for {
		var candidate rbacv1.ClusterRole
		err := decoder.Decode(&candidate)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if candidate.Kind == "ClusterRole" && candidate.Name == "hybridspot-management" {
			role = candidate
		}
	}
	if role.Name == "" {
		t.Fatal("management ClusterRole missing")
	}
	contains := func(values []string, value string) bool {
		for _, item := range values {
			if item == value || item == "*" {
				return true
			}
		}
		return false
	}
	allowed := func(resource, verb string) bool {
		for _, rule := range role.Rules {
			if contains(rule.APIGroups, "training.dcnlab.com") && contains(rule.Resources, resource) && contains(rule.Verbs, verb) {
				return true
			}
		}
		return false
	}
	for _, resource := range []string{"trainingpolicies", "spotriskprofiles"} {
		for _, verb := range []string{"create", "update", "delete"} {
			if allowed(resource, verb) {
				t.Errorf("management must not %s user-owned %s", verb, resource)
			}
		}
		if !allowed(resource, "watch") || !allowed(resource+"/status", "patch") {
			t.Errorf("missing observation/status permissions for %s", resource)
		}
	}
	if allowed("spotriskprofiles", "patch") {
		t.Fatal("collector must not patch risk input spec")
	}
	if !allowed("trainingpolicies", "patch") || !allowed("trainingruntimes", "create") {
		t.Fatal("missing policy intent annotation or runtime creation permissions")
	}
}
