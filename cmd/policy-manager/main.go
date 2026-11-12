package main

import (
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
)

func main() {
	// Deprecated alias. Do not co-run with canonical deployments.
	app.Run("spot-vm-management-controller", management.SetupVMManagement)
}
