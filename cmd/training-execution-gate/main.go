// The gate runs as an init container and cannot write any Kubernetes object.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/framework"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	var path, ns, name, uid, workUID string
	flag.StringVar(&path, "kubeconfig", "", "read-only Run credential")
	flag.StringVar(&ns, "namespace", "", "Run namespace")
	flag.StringVar(&name, "run", "", "Run name")
	flag.StringVar(&uid, "run-uid", "", "Run UID")
	flag.StringVar(&workUID, "workload-uid", "", "workload UID")
	flag.Parse()
	podName, podUID := os.Getenv("POD_NAME"), os.Getenv("POD_UID")
	if path == "" || ns == "" || name == "" || uid == "" || workUID == "" || podName == "" || podUID == "" {
		fmt.Fprintln(os.Stderr, "execution gate identity/configuration missing")
		os.Exit(1)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "execution gate credential unavailable")
		os.Exit(1)
	}
	cfg.Timeout = 10 * time.Second
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "execution gate API client unavailable")
		os.Exit(1)
	}
	for attempt := 0; ; attempt++ {
		run := resource.Object("TrainingRun")
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, run)
		if err == nil && framework.ExecutionPermitted(run, uid, workUID, podName, podUID) {
			fmt.Println("UID-bound execution permit verified; starting this Pod's workload")
			return
		}
		if err == nil && (string(run.GetUID()) != uid || !run.GetDeletionTimestamp().IsZero()) {
			fmt.Fprintln(os.Stderr, "execution Run replaced or deleting")
			os.Exit(1)
		}
		if attempt%12 == 0 {
			fmt.Println("waiting for UID-bound execution permit")
		}
		time.Sleep(5 * time.Second)
	}
}
