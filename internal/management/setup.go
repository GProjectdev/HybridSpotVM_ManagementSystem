package management

import (
	"fmt"
	ctrl "sigs.k8s.io/controller-runtime"
)

type managedController interface{ SetupWithManager(ctrl.Manager) error }

// These are process ownership boundaries, not wire API renames. In particular,
// TrainingPolicy and NodeProvision remain the compatibility contracts.
func componentControllers(mgr ctrl.Manager, component string) ([]managedController, error) {
	c, reader := mgr.GetClient(), mgr.GetAPIReader()
	switch component {
	case "spot-vm-management-controller":
		return []managedController{
			&PolicyReconciler{Client: c, APIReader: reader, Clock: defaultClock, GroupStage: groupStageIntent},
			&ReplacementCapacityReconciler{Client: c, APIReader: reader, Clock: defaultClock},
			&RecoveryReconciler{Client: c, APIReader: reader, Clock: defaultClock},
			&GroupRecoveryReconciler{Client: c, Reader: reader},
			&GroupCapacityReconciler{&PolicyReconciler{Client: c, APIReader: reader, Clock: defaultClock}},
		}, nil
	case "training-lifecycle-controller":
		return []managedController{
			&DiscoveryReconciler{Client: c, Reader: reader},
			&GroupPlacementReconciler{Client: c, Reader: reader, ExistingNodes: existingNodeResolver(reader), ExistingClaims: existingClaimPreparer(reader)},
		}, nil
	case "checkpoint-recovery-coordinator":
		return []managedController{
			&ReplacementReconciler{Client: c, APIReader: reader, Clock: defaultClock},
			&CheckpointReconciler{Client: c, Clock: defaultClock, GroupStage: groupStageIntent},
			&GroupRequestReconciler{&PolicyReconciler{Client: c, APIReader: reader, Clock: defaultClock}},
		}, nil
	default:
		return nil, fmt.Errorf("unknown management component %q", component)
	}
}

func setupComponent(mgr ctrl.Manager, component string) error {
	controllers, err := componentControllers(mgr, component)
	if err != nil {
		return err
	}
	for _, controller := range controllers {
		if err := controller.SetupWithManager(mgr); err != nil {
			return err
		}
	}
	return nil
}

// SetupVMManagement owns baseline and replacement capacity. It does not write
// SpotReplacement status or produce replacement restore/cleanup requests.
func SetupVMManagement(mgr ctrl.Manager) error {
	return setupComponent(mgr, "spot-vm-management-controller")
}

// SetupLifecycle owns workload discovery, runtime identity/propagation and
// placement transitions. Group placement retains its fenced target bootstrap;
// PolicyReconciler's source/target gate excludes that transition from allocation.
func SetupLifecycle(mgr ctrl.Manager) error {
	return setupComponent(mgr, "training-lifecycle-controller")
}

// SetupCheckpointRecovery owns checkpoint and restore/cleanup requests.
// VM management validates recovery evidence and owns UID-bound retirement.
func SetupCheckpointRecovery(mgr ctrl.Manager) error {
	return setupComponent(mgr, "checkpoint-recovery-coordinator")
}

// Deprecated: use SetupVMManagement, with SetupLifecycle in its own process.
// Never co-run legacy binaries with canonical deployments.
func SetupPolicy(mgr ctrl.Manager) error { return SetupVMManagement(mgr) }

// Deprecated: use SetupCheckpointRecovery. Both old coordinator/recovery command
// names are aliases of that entire process and must not be deployed together.
func SetupCheckpoint(mgr ctrl.Manager) error { return SetupCheckpointRecovery(mgr) }
func SetupRecovery(mgr ctrl.Manager) error   { return SetupCheckpointRecovery(mgr) }
