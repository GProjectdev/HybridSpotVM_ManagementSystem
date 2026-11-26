package framework

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FluidCR control files belong to the whole world, not to the rank checkpoint
// filename. Distinct filenames on the same NFS root cannot isolate two Runs.
func checkpointVolumeIdentity(ctx context.Context, c client.Reader, members MemberReader, work *unstructured.Unstructured, cluster string) (map[string]interface{}, error) {
	in := p.PolicyInput{Namespace: work.GetNamespace(), WorkloadRef: p.WorkloadRef{Name: work.GetName(), UID: work.GetUID()}}
	claim, _, err := management.ReadGroupCheckpointVolume(ctx, c, in)
	if err != nil {
		return nil, err
	}
	return sharedVolume(ctx, members, work.GetNamespace(), claim, cluster, cluster)
}
func overlappingNFS(a, b map[string]interface{}) bool {
	if a["server"] != b["server"] {
		return false
	}
	x, y := path.Clean(fmt.Sprint(a["path"])), path.Clean(fmt.Sprint(b["path"]))
	return x == y || strings.HasPrefix(x, strings.TrimRight(y, "/")+"/") || strings.HasPrefix(y, strings.TrimRight(x, "/")+"/")
}
func checkpointIsolation(ctx context.Context, c client.Reader, members MemberReader, run, work *unstructured.Unstructured, cluster string) error {
	runs, err := listItems(ctx, c, "training.dcnlab.com/v1alpha1", "TrainingRun", run.GetNamespace())
	if err != nil {
		return err
	}
	var own map[string]interface{}
	for i := range runs {
		other := &runs[i]
		if other.GetUID() == run.GetUID() || str(other, "spec", "mode") != "Execute" {
			continue
		}
		w := object("apps/v1", "StatefulSet")
		err = c.Get(ctx, key(other.GetNamespace(), str(other, "spec", "workloadRef", "name")), w)
		if apierrors.IsNotFound(err) && str(other, "status", "committedCluster") == "" {
			continue
		}
		if err != nil {
			return err
		}
		member := str(other, "status", "committedCluster")
		if member == "" {
			member = str(other, "spec", "onPremiseCluster")
		}
		if boolAt(other, "spec", "suspend") && integer(w, "spec", "replicas") == 0 {
			reader, e := members(member)
			if e != nil {
				return e
			}
			pods := &corev1.PodList{}
			if e = reader.List(ctx, pods, client.InNamespace(run.GetNamespace())); e != nil {
				return e
			}
			present := false
			for _, pod := range pods.Items {
				for _, owner := range pod.OwnerReferences {
					if owner.Kind == "StatefulSet" && owner.Name == w.GetName() {
						present = true
					}
				}
			}
			if !present {
				continue
			}
		}
		if string(w.GetUID()) != str(other, "spec", "workloadRef", "uid") {
			return fmt.Errorf("other Run storage owner changed")
		}
		if own == nil {
			own, err = checkpointVolumeIdentity(ctx, c, members, work, cluster)
			if err != nil {
				return err
			}
		}
		foreign, e := checkpointVolumeIdentity(ctx, c, members, w, member)
		if e != nil {
			return fmt.Errorf("cannot verify storage isolation from %s: %w", other.GetName(), e)
		}
		if overlappingNFS(own, foreign) {
			return fmt.Errorf("checkpoint NFS control directory overlaps Run %s; use a dedicated PVC/NFS directory per training world", other.GetName())
		}
	}
	return nil
}
