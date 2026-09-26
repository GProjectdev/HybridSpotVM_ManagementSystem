package management

import ctrl "sigs.k8s.io/controller-runtime"

func Setup(mgr ctrl.Manager) error {
	if err := (&PolicyReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Clock: defaultClock}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&CheckpointReconciler{Client: mgr.GetClient(), Clock: defaultClock}).SetupWithManager(mgr); err != nil {
		return err
	}
	return (&RecoveryReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Clock: defaultClock}).SetupWithManager(mgr)
}
