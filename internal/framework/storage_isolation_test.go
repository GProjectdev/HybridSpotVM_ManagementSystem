package framework

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

func TestCheckpointIsolationUsesPhysicalNFSRootNotPVCName(t *testing.T) {
	for _, tc := range []struct {
		name, root string
		blocked    bool
	}{{"same-root-different-claims", "/nfs/a", true}, {"ancestor", "/nfs", true}, {"nested", "/nfs/a/child", true}, {"distinct", "/nfs/b", false}, {"prefix-only", "/nfs/ab", false}} {
		t.Run(tc.name, func(t *testing.T) {
			a, wa := initialObjects("Execute")
			b, wb := initialObjects("Execute")
			b.SetName("b")
			b.SetUID("run-b")
			wb.SetName("b")
			wb.SetUID("source-b")
			for i, pair := range [][2]*unstructured.Unstructured{{a, wa}, {b, wb}} {
				r, w := pair[0], pair[1]
				_ = unstructured.SetNestedField(r.Object, w.GetName(), "spec", "workloadRef", "name")
				_ = unstructured.SetNestedField(r.Object, string(w.GetUID()), "spec", "workloadRef", "uid")
				claim := []string{"claim-a", "claim-b"}[i]
				_ = unstructured.SetNestedMap(w.Object, map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "trainer", "env": []interface{}{map[string]interface{}{"name": "FLUIDCR_CHECKPOINT_DIR", "value": "/checkpoint"}}, "volumeMounts": []interface{}{map[string]interface{}{"name": "checkpoint", "mountPath": "/checkpoint"}}}}, "volumes": []interface{}{map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": claim}}}}, "spec", "template", "spec")
			}
			c := frameworkClient(a, b, wa, wb)
			m := frameworkClient()
			for i, root := range []string{"/nfs/a", tc.root} {
				name := []string{"claim-a", "claim-b"}[i]
				uid := types.UID(name)
				pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Namespace: "ns", Name: name, UID: uid}, PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.1", Path: root}}}}
				pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: uid}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: name}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
				if e := m.Create(context.Background(), pv); e != nil {
					t.Fatal(e)
				}
				if e := m.Create(context.Background(), pvc); e != nil {
					t.Fatal(e)
				}
			}
			e := checkpointIsolation(context.Background(), c, func(string) (client.Reader, error) { return m, nil }, a, wa, "onpre1")
			if (e != nil) != tc.blocked {
				t.Fatalf("blocked=%v error=%v", tc.blocked, e)
			}
		})
	}
}
