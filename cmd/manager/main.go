package main

import (
	"flag"
	"os"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/collector"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/member"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var mode, metrics, health string
	var leader bool
	flag.StringVar(&mode, "mode", "management", "management, runtime, or spot-watcher")
	flag.StringVar(&metrics, "metrics-bind-address", ":8080", "metrics listener")
	flag.StringVar(&health, "health-probe-bind-address", ":8081", "health listener")
	flag.BoolVar(&leader, "leader-elect", true, "leader election; disabled in spot-watcher mode")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	if mode == "spot-watcher" {
		leader = false
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: metrics}, HealthProbeBindAddress: health, LeaderElection: leader, LeaderElectionID: "hybridspot-" + mode, LeaderElectionNamespace: os.Getenv("POD_NAMESPACE")})
	must(err)
	switch mode {
	case "management":
		must(collector.Setup(mgr))
		must(management.Setup(mgr))
	case "runtime":
		must(member.SetupRuntime(mgr))
	case "spot-watcher":
		must(mgr.Add(&member.SpotWatcher{Client: mgr.GetClient(), NodeName: os.Getenv("NODE_NAME")}))
	default:
		ctrl.Log.Error(nil, "invalid mode", "mode", mode)
		os.Exit(2)
	}
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
