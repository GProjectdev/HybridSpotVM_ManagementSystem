package main

import (
	"os"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/member"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	app.Run("spot-watcher", func(mgr ctrl.Manager) error {
		return mgr.Add(&member.SpotWatcher{Client: mgr.GetClient(), NodeName: os.Getenv("NODE_NAME")})
	})
}
