package app

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func leaderElectionID(component string) string {
	return "hybridspot-" + component
}

// Run starts only the controller registered by this component's executable.
func Run(component string, setup func(ctrl.Manager) error) {
	var metrics, health string
	var leader bool
	flag.StringVar(&metrics, "metrics-bind-address", ":8080", "metrics listener")
	flag.StringVar(&health, "health-probe-bind-address", ":8081", "health listener")
	flag.BoolVar(&leader, "leader-elect", component != "spot-watcher", "enable leader election (always disabled for node-local spot-watcher)")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)).WithName(component))
	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	if component == "spot-watcher" {
		leader = false
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metrics},
		HealthProbeBindAddress:  health,
		LeaderElection:          leader,
		LeaderElectionID:        leaderElectionID(component),
		LeaderElectionNamespace: os.Getenv("POD_NAMESPACE"),
	})
	must(err)
	must(setup(mgr))
	must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(mgr.AddReadyzCheck("readyz", healthz.Ping))
	must(mgr.Start(ctrl.SetupSignalHandler()))
}

func must(err error) {
	if err != nil {
		ctrl.Log.Error(err, "startup failed")
		os.Exit(1)
	}
}
