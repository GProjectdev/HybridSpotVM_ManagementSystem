package management

import (
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

type registrationManager struct{ ctrl.Manager }

func (registrationManager) GetClient() client.Client    { return nil }
func (registrationManager) GetAPIReader() client.Reader { return nil }
func TestCanonicalControllerOwnership(t *testing.T) {
	want := map[string][]string{
		"spot-vm-management-controller":   {"*management.PolicyReconciler", "*management.ReplacementCapacityReconciler", "*management.RecoveryReconciler", "*management.GroupRecoveryReconciler", "*management.GroupCapacityReconciler"},
		"training-lifecycle-controller":   {"*management.DiscoveryReconciler", "*management.GroupPlacementReconciler"},
		"checkpoint-recovery-coordinator": {"*management.ReplacementReconciler", "*management.CheckpointReconciler", "*management.GroupRequestReconciler"},
	}
	owners := map[string]string{}
	for component, expected := range want {
		controllers, err := componentControllers(registrationManager{}, component)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, controller := range controllers {
			name := reflect.TypeOf(controller).String()
			if previous := owners[name]; previous != "" {
				t.Fatalf("%s registered by both %s and %s", name, previous, component)
			}
			owners[name] = component
			got = append(got, name)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s: got %v, want %v", component, got, expected)
		}
	}
	if _, err := componentControllers(registrationManager{}, "unknown"); err == nil {
		t.Fatal("unknown component accepted")
	}
}
