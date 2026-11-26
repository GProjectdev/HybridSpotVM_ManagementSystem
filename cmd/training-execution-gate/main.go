// The gate runs as an init container and cannot write any Kubernetes object.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/framework"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type options struct{ path, ns, name, uid, workUID string }

func parseOptions(args []string) (options, error) {
	var o options
	// controller-runtime registers kubeconfig on the global FlagSet during
	// import. A dedicated command FlagSet avoids duplicate registration panics.
	f := flag.NewFlagSet("training-execution-gate", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.path, "kubeconfig", "", "read-only Run credential")
	f.StringVar(&o.ns, "namespace", "", "Run namespace")
	f.StringVar(&o.name, "run", "", "Run name")
	f.StringVar(&o.uid, "run-uid", "", "Run UID")
	f.StringVar(&o.workUID, "workload-uid", "", "workload UID")
	err := f.Parse(args)
	return o, err
}

func main() {
	o, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid execution gate arguments")
		os.Exit(1)
	}
	path, ns, name, uid, workUID := o.path, o.ns, o.name, o.uid, o.workUID
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
