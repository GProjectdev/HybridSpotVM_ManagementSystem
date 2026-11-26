package framework

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sharedVolume requires the same durable storage identity on both members. It
// does not manufacture PV migration receipts or assume equal PVC names suffice.
func sharedVolume(ctx context.Context, members MemberReader, ns, name, source, target string) (map[string]interface{}, error) {
	var identity map[string]interface{}
	for _, cluster := range []string{source, target} {
		reader, e := members(cluster)
		if e != nil {
			return nil, e
		}
		claim := &corev1.PersistentVolumeClaim{}
		if e = reader.Get(ctx, key(ns, name), claim); e != nil {
			return nil, e
		}
		if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" || !claim.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("checkpoint PVC not Bound in %s", cluster)
		}
		pv := &corev1.PersistentVolume{}
		if e = reader.Get(ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, pv); e != nil {
			return nil, e
		}
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != claim.UID || pv.Spec.ClaimRef.Namespace != ns || pv.Spec.ClaimRef.Name != name || pv.Spec.NFS == nil || !pv.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("reciprocal NFS PV/PVC identity required in %s", cluster)
		}
		current := map[string]interface{}{"server": pv.Spec.NFS.Server, "path": pv.Spec.NFS.Path}
		if current["server"] == "" || current["path"] == "" || pv.Spec.NFS.ReadOnly {
			return nil, fmt.Errorf("writable NFS checkpoint volume required")
		}
		if identity != nil && !reflect.DeepEqual(identity, current) {
			return nil, fmt.Errorf("cross-member checkpoint volumes differ; verified PV migration is required")
		}
		identity = current
	}
	return identity, nil
}

func (r *TransitionReconciler) prepareTargets(ctx context.Context, t, run, work *unstructured.Unstructured, rs RunSpec, ts TransitionSpec, cap *unstructured.Unstructured, pvc string) (bool, error) {
	volume, e := sharedVolume(ctx, r.Members, run.GetNamespace(), pvc, ts.SourceCluster, ts.TargetCluster)
	if e != nil {
		return false, e
	}
	reader, e := r.Members(ts.TargetCluster)
	if e != nil {
		return false, e
	}
	containers, _, _ := unstructured.NestedSlice(work.Object, "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		return false, fmt.Errorf("one trainer image required for preparation")
	}
	image, _ := containers[0].(map[string]interface{})["image"].(string)
	if !strings.Contains(image, "@sha256:") {
		return false, fmt.Errorf("pinned trainer image digest required")
	}
	sources := map[string]bool{}
	for _, p := range ts.SourcePods {
		sources[p.NodeName] = true
	}
	for rank, name := range ts.TargetAllocations {
		nodeName := str(cap, "status", "allocations", name, "nodeName")
		node := &corev1.Node{}
		if e = reader.Get(ctx, client.ObjectKey{Name: nodeName}, node); e != nil {
			return false, e
		}
		if string(node.UID) != str(cap, "status", "allocations", name, "nodeUID") || node.Labels["ml.dcn.ssu.ac.kr/node-provision-uid"] != str(cap, "status", "allocations", name, "memberUID") || !node.DeletionTimestamp.IsZero() {
			return false, fmt.Errorf("target node incarnation changed")
		}
		if sources[nodeName] && ts.SourceCluster == ts.TargetCluster {
			continue
		} // current world already uses this image/storage/GPU
		job := object("batch/v1", "Job")
		job.SetNamespace(run.GetNamespace())
		job.SetName(fmt.Sprintf("%s-probe-%d", t.GetName(), rank))
		script := "import os,torch; assert torch.cuda.is_available(); x=torch.ones(1,device='cuda'); assert x.item()==1; p='/checkpoint/.framework-probes/'+os.environ['OPERATION']; os.makedirs(p,exist_ok=True); f=p+'/'+os.environ['NODE_UID']; fd=os.open(f,os.O_CREAT|os.O_RDWR,0o600); os.write(fd,b'probe'); os.fsync(fd); os.close(fd); print('TARGET_IMAGE_GPU_STORAGE_READY',flush=True)"
		pod := map[string]interface{}{"nodeName": nodeName, "restartPolicy": "Never", "automountServiceAccountToken": false, "containers": []interface{}{map[string]interface{}{"name": "probe", "image": image, "command": []interface{}{"python", "-c", script}, "env": []interface{}{map[string]interface{}{"name": "OPERATION", "value": string(t.GetUID())}, map[string]interface{}{"name": "NODE_UID", "value": string(node.UID)}}, "resources": map[string]interface{}{"requests": map[string]interface{}{"cpu": "100m", "memory": "256Mi", "nvidia.com/gpu": "1"}, "limits": map[string]interface{}{"cpu": "1", "memory": "1Gi", "nvidia.com/gpu": "1"}}, "volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}}}}, "volumes": []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": pvc}}}}
		runtime := str(work, "spec", "template", "spec", "runtimeClassName")
		if rs.Cloud.RuntimeClassName != nil {
			runtime = *rs.Cloud.RuntimeClassName
		}
		if runtime != "" {
			pod["runtimeClassName"] = runtime
		}
		secrets, found, _ := unstructured.NestedSlice(work.Object, "spec", "template", "spec", "imagePullSecrets")
		if found {
			pod["imagePullSecrets"] = secrets
		}
		job.Object["spec"] = map[string]interface{}{"backoffLimit": int64(0), "activeDeadlineSeconds": int64(600), "template": map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{RunUIDLabel: string(run.GetUID())}, "annotations": map[string]interface{}{"fluidcr.dcnlab.com/inject": "false"}}, "spec": pod}}
		job.SetAnnotations(map[string]string{"training.dcnlab.com/target-node-uid": string(node.UID), "training.dcnlab.com/volume-server": volume["server"].(string), "training.dcnlab.com/volume-path": volume["path"].(string)})
		setOwner(job, t)
		live := object("batch/v1", "Job")
		e = r.Get(ctx, client.ObjectKeyFromObject(job), live)
		if apierrors.IsNotFound(e) {
			if e = r.Create(ctx, job); e != nil {
				return false, e
			}
			return false, ensurePlacement(ctx, r.Client, t, job, []string{ts.TargetCluster})
		}
		if e != nil {
			return false, e
		}
		if !owned(live, t) || live.GetAnnotations()["training.dcnlab.com/target-node-uid"] != string(node.UID) || !containsSpec(live.Object["spec"], job.Object["spec"]) {
			return false, fmt.Errorf("preparation job immutable identity mismatch")
		}
		if e = ensurePlacement(ctx, r.Client, t, live, []string{ts.TargetCluster}); e != nil {
			return false, e
		}
		local := object("batch/v1", "Job")
		if e = reader.Get(ctx, client.ObjectKeyFromObject(live), local); apierrors.IsNotFound(e) {
			return false, nil
		} else if e != nil {
			return false, e
		}
		if local.GetAnnotations()["resourcetemplate.karmada.io/uid"] != string(live.GetUID()) || str(local, "spec", "template", "spec", "nodeName") != nodeName {
			return false, fmt.Errorf("member preparation job provenance changed")
		}
		if integer(local, "status", "failed") > 0 {
			return false, fmt.Errorf("target image/GPU/storage probe failed on %s", nodeName)
		}
		if integer(local, "status", "succeeded") != 1 {
			return false, nil
		}
	}
	return true, nil
}
