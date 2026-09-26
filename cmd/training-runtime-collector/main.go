package main

import (
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/member"
)

func main() {
	app.Run("training-runtime-collector", member.SetupRuntime)
}
