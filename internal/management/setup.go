package management

import ctrl "sigs.k8s.io/controller-runtime"

func SetupPolicy(mgr ctrl.Manager) error {
	return (&PolicyReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Clock: defaultClock}).SetupWithManager(mgr)
}

func SetupCheckpoint(mgr ctrl.Manager) error {
	return (&CheckpointReconciler{Client: mgr.GetClient(), Clock: defaultClock}).SetupWithManager(mgr)
}

func SetupRecovery(mgr ctrl.Manager) error {
	return (&RecoveryReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Clock: defaultClock}).SetupWithManager(mgr)
}
