package main

import (
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/collector"
)

func main() { app.Run("public-cloud-vm-collector", collector.Setup) }
