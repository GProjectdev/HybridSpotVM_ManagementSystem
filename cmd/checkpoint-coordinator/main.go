package main

import (
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
)

func main() {
	app.Run("checkpoint-coordinator", management.SetupCheckpoint)
}
