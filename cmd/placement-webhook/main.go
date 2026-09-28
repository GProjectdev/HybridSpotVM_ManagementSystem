package main

import (
	"fmt"
	"os"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func main() {
	app.Run("placement-webhook", func(mgr ctrl.Manager) error {
		username := os.Getenv("PLACEMENT_RELEASE_USERNAME")
		if username == "" {
			return fmt.Errorf("PLACEMENT_RELEASE_USERNAME is required")
		}
		mgr.GetWebhookServer().Register("/hold-ddp-placement", &admission.Webhook{Handler: &management.PlacementAdmission{Reader: mgr.GetAPIReader(), ReleaseUsername: username}})
		return nil
	})
}
