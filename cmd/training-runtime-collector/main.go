package main

import (
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/member"
)

func main() {
	// Deprecated alias. Do not co-run with canonical deployments.
	app.Run("training-runtime-controller", member.SetupRuntime)
}
