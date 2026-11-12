package main

import (
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
)

func main() {
	// Deprecated alias for the integrated coordinator, not a second recovery owner.
	// Do not co-run with checkpoint-coordinator or canonical deployments.
	app.Run("checkpoint-recovery-coordinator", management.SetupCheckpointRecovery)
}
