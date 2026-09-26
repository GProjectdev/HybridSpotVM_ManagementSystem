package main

import (
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/collector"
)

func main() {
	app.Run("vm-spot-risk-collector", collector.Setup)
}
