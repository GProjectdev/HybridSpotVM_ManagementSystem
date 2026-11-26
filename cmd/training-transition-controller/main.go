package main

import (
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/app"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/framework"
)

func main() { app.Run("training-transition-controller", framework.SetupTransition) }
