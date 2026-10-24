package management

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var memberFileName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Only an explicitly mounted directory enables direct member access. There is no
// ambient-context fallback and no Secret lookup or credential logging.
func existingMemberLoader(directory string) func(string) (client.Client, error) {
	var members sync.Map
	return func(cluster string) (client.Client, error) {
		if directory == "" || !filepath.IsAbs(directory) || !memberFileName.MatchString(cluster) {
			return nil, fmt.Errorf("existing capacity requires an absolute kubeconfig directory and a valid target cluster")
		}
		var member client.Client
		if cached, ok := members.Load(cluster); ok {
			member = cached.(client.Client)
		} else {
			config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(directory, cluster+".kubeconfig"))
			if err != nil {
				return nil, fmt.Errorf("cannot load configured existing-capacity member kubeconfig")
			}
			config.Timeout = 15 * time.Second
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				return nil, err
			}
			c, err := client.New(config, client.Options{Scheme: scheme})
			if err != nil {
				return nil, fmt.Errorf("cannot construct existing-capacity member reader")
			}
			member = c
			members.Store(cluster, member)
		}
		return member, nil
	}
}

func newExistingNodeResolver(reader client.Reader, directory string) func(context.Context, p.PolicyInput) (map[string]string, error) {
	load := existingMemberLoader(directory)
	return func(ctx context.Context, input p.PolicyInput) (map[string]string, error) {
		cluster := input.Capacity.AWSCluster
		member, err := load(cluster)
		if err != nil {
			return nil, err
		}
		sts := p.NewObject("StatefulSet")
		if err := reader.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.WorkloadRef.Name}, sts); err != nil {
			return nil, err
		}
		if string(sts.GetUID()) != string(input.WorkloadRef.UID) || !sts.GetDeletionTimestamp().IsZero() {
			return nil, fmt.Errorf("existing-capacity workload identity changed")
		}
		typed := &appsv1.StatefulSet{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(sts.Object, typed); err != nil {
			return nil, err
		}
		gpu := corev1.ResourceName("nvidia.com/gpu")
		var demand int64
		for _, container := range typed.Spec.Template.Spec.Containers {
			quantity := container.Resources.Limits[gpu]
			demand += quantity.Value()
		}
		for _, container := range typed.Spec.Template.Spec.InitContainers {
			quantity := container.Resources.Limits[gpu]
			if quantity.Sign() != 0 {
				return nil, fmt.Errorf("GPU init containers are unsupported in existing-capacity failover")
			}
		}
		if demand <= 0 {
			return nil, fmt.Errorf("existing-capacity workload must request NVIDIA GPUs")
		}
		return ExistingCapacityNodes(ctx, member, input, cluster, demand)
	}
}

func existingNodeResolver(reader client.Reader) func(context.Context, p.PolicyInput) (map[string]string, error) {
	return newExistingNodeResolver(reader, os.Getenv("EXISTING_CAPACITY_KUBECONFIG_DIR"))
}

func existingClaimPreparer(reader client.Reader) func(context.Context, p.PolicyInput, *unstructured.Unstructured, *unstructured.Unstructured) (bool, error) {
	load := existingMemberLoader(os.Getenv("EXISTING_CAPACITY_KUBECONFIG_DIR"))
	return func(ctx context.Context, input p.PolicyInput, binding, request *unstructured.Unstructured) (bool, error) {
		if input.Capacity.Mode != p.CapacityModeExisting {
			return false, fmt.Errorf("Existing capacity opt-in required")
		}
		// Repeat the receipt validation at the write boundary.
		if err := validateGroupSourceFence(ctx, reader, request); err != nil {
			return false, err
		}
		if err := validateGroupVolumes(ctx, reader, binding, request); err != nil {
			return false, err
		}
		sts := p.NewObject("StatefulSet")
		if err := reader.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: input.WorkloadRef.Name}, sts); err != nil {
			return false, err
		}
		templates, _, err := unstructured.NestedSlice(sts.Object, "spec", "volumeClaimTemplates")
		if err != nil {
			return false, err
		}
		if len(templates) == 0 {
			return true, nil
		}
		migration := &unstructured.Unstructured{}
		migration.SetGroupVersionKind(schema.GroupVersionKind{Group: "migration.dcnlab.com", Version: "v1alpha1", Kind: "PVMigration"})
		if err := reader.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: binding.GetAnnotations()["migration.dcnlab.com/pv-migration"]}, migration); err != nil {
			return false, err
		}
		metadata := &unstructured.Unstructured{}
		metadata.SetGroupVersionKind(migration.GroupVersionKind().GroupVersion().WithKind("PVMetadata"))
		if err := reader.Get(ctx, client.ObjectKey{Namespace: input.Namespace, Name: stringField(migration.Object, "spec", "metadataRef")}, metadata); err != nil {
			return false, err
		}
		member, err := load(input.Capacity.AWSCluster)
		if err != nil {
			return false, err
		}
		return EnsureExistingTargetClaims(ctx, member, sts, migration, metadata, request)
	}
}
