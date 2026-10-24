package management

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type existingClaimsFixture struct {
	sts, migration, metadata, request *unstructured.Unstructured
	pvs                               []*corev1.PersistentVolume
	claims                            []*corev1.PersistentVolumeClaim
}

func newExistingClaimsFixture(t *testing.T) *existingClaimsFixture {
	t.Helper()
	encode := func(value interface{}) map[string]interface{} {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	sts := &appsv1.StatefulSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}, ObjectMeta: metav1.ObjectMeta{Namespace: "training", Name: "trainer", UID: "workload-uid"}}
	f := &existingClaimsFixture{}
	volumes, mappings, works := []interface{}{}, []interface{}{}, []interface{}{}
	for i, name := range []string{"checkpoint", "data"} {
		class := "shared"
		template := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app": "trainer"}}, Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		}}
		sts.Spec.VolumeClaimTemplates = append(sts.Spec.VolumeClaimTemplates, template)
		claim := name + "-trainer-0"
		source := corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: class, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs.internal", Path: "/exports/" + name}},
			ClaimRef:               &corev1.ObjectReference{Namespace: "training", Name: claim, UID: types.UID("source-" + name)},
		}
		volumes = append(volumes, map[string]interface{}{"pvcName": claim, "pvcUID": "source-" + name, "templateName": name, "ordinal": int64(0), "pvName": "source-pv-" + name, "pvSpec": encode(&source)})
		mappings = append(mappings, map[string]interface{}{"sourcePVC": claim, "targetPVC": claim})
		pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("receipt-pv-%d", i), UID: types.UID(fmt.Sprintf("target-pv-uid-%d", i)), Labels: map[string]string{
			existingClaimPVOwnerLabel: "migration-uid", existingClaimPVManagedLabel: existingClaimPVManager,
		}}, Spec: *source.DeepCopy(), Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable}}
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		pv.Spec.ClaimRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: "training", Name: claim}
		f.pvs = append(f.pvs, pv)
		works = append(works, map[string]interface{}{"name": pv.Name, "namespace": "karmada-es-aws", "applied": true, "detached": true})
	}
	f.sts = &unstructured.Unstructured{Object: encode(sts)}
	f.request = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "migration.dcnlab.com/v1alpha1", "kind": "RestoreRequest",
		"metadata": map[string]interface{}{"namespace": "training", "name": "restore", "uid": "request-uid"},
		"spec":     map[string]interface{}{"sourceCluster": "onprem", "targetCluster": "aws", "workloadRef": map[string]interface{}{"name": "trainer", "uid": "workload-uid"}, "pods": []interface{}{map[string]interface{}{"sourcePod": "trainer-0"}}},
	}}
	f.migration = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "migration.dcnlab.com/v1alpha1", "kind": "PVMigration",
		"metadata": map[string]interface{}{"namespace": "training", "name": "migration", "uid": "migration-uid", "generation": int64(2), "annotations": map[string]interface{}{placementRequestUIDAnnotation: "request-uid"}},
		"spec":     map[string]interface{}{"sourceCluster": "onprem", "targetCluster": "aws", "metadataRef": "snapshot", "sourceFenced": true, "volumes": mappings},
		"status":   map[string]interface{}{"phase": "Completed", "observedGeneration": int64(2), "planHash": "recorded-plan", "works": works},
	}}
	f.metadata = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "migration.dcnlab.com/v1alpha1", "kind": "PVMetadata",
		"metadata": map[string]interface{}{"namespace": "training", "name": "snapshot", "uid": "metadata-uid", "generation": int64(3)},
		"spec":     map[string]interface{}{"sourceCluster": "onprem", "workloadRef": map[string]interface{}{"name": "trainer", "uid": "workload-uid"}},
		"status":   map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "onprem", "workloadUID": "source-member-workload-uid", "observedGeneration": int64(3), "collectedAt": "2026-10-24T00:00:00Z", "volumes": volumes}}},
	}}
	return f
}

func (f *existingClaimsFixture) snapshot() map[string]interface{} {
	return f.metadata.Object["status"].(map[string]interface{})["clusters"].([]interface{})[0].(map[string]interface{})
}
func (f *existingClaimsFixture) work() map[string]interface{} {
	return f.migration.Object["status"].(map[string]interface{})["works"].([]interface{})[0].(map[string]interface{})
}
func (f *existingClaimsFixture) sourceVolume() map[string]interface{} {
	return f.snapshot()["volumes"].([]interface{})[0].(map[string]interface{})
}
func (f *existingClaimsFixture) addClaim(t *testing.T, index int, bound bool) {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(f.sts.Object, &sts); err != nil {
		t.Fatal(err)
	}
	pv := f.pvs[index]
	claim, err := existingClaimFromTemplate(&sts.Spec.VolumeClaimTemplates[index], pv, "training", pv.Spec.ClaimRef.Name, "request-uid")
	if err != nil {
		t.Fatal(err)
	}
	claim.UID = types.UID("claim-" + claim.Name)
	claim.Status.Phase = corev1.ClaimPending
	if bound {
		claim.Status.Phase = corev1.ClaimBound
		pv.Spec.ClaimRef.UID = claim.UID
		pv.Status.Phase = corev1.VolumeBound
	}
	f.claims = append(f.claims, claim)
}

type existingClaimsClient struct {
	client.Client
	creates, mutations          int
	failPV, failPVC, failCreate bool
}

func (c *existingClaimsClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	_, pv := obj.(*corev1.PersistentVolume)
	if (pv && c.failPV) || (!pv && c.failPVC) {
		return fmt.Errorf("member read forbidden")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func (c *existingClaimsClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); !ok {
		return fmt.Errorf("non-PVC create forbidden")
	}
	c.creates++
	if c.failCreate {
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, obj.GetName())
	}
	obj.SetUID(types.UID("created-" + obj.GetName()))
	return c.Client.Create(ctx, obj, opts...)
}
func (c *existingClaimsClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	c.mutations++
	return fmt.Errorf("update forbidden")
}
func (c *existingClaimsClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	c.mutations++
	return fmt.Errorf("patch forbidden")
}
func (c *existingClaimsClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	c.mutations++
	return fmt.Errorf("delete forbidden")
}
func (c *existingClaimsClient) DeleteAllOf(context.Context, client.Object, ...client.DeleteAllOfOption) error {
	c.mutations++
	return fmt.Errorf("delete forbidden")
}

func (f *existingClaimsFixture) member(t *testing.T) *existingClaimsClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{}
	for _, pv := range f.pvs {
		objects = append(objects, pv)
	}
	for _, claim := range f.claims {
		objects = append(objects, claim)
	}
	return &existingClaimsClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.PersistentVolume{}, &corev1.PersistentVolumeClaim{}).WithObjects(objects...).Build()}
}
func (f *existingClaimsFixture) ensure(c client.Client) (bool, error) {
	return EnsureExistingTargetClaims(context.Background(), c, f.sts, f.migration, f.metadata, f.request)
}

func TestExistingTargetClaimsCreateAllThenBound(t *testing.T) {
	f := newExistingClaimsFixture(t)
	c := f.member(t)
	before := []*unstructured.Unstructured{f.sts.DeepCopy(), f.migration.DeepCopy(), f.metadata.DeepCopy(), f.request.DeepCopy()}
	if ready, err := f.ensure(c); ready || err != nil {
		t.Fatalf("create: ready=%v err=%v", ready, err)
	}
	if c.creates != len(f.pvs) {
		t.Fatalf("created %d, want all %d", c.creates, len(f.pvs))
	}
	if ready, err := f.ensure(c); ready || err != nil {
		t.Fatalf("pending: ready=%v err=%v", ready, err)
	}
	ctx := context.Background()
	for _, expected := range f.pvs {
		var claim corev1.PersistentVolumeClaim
		if err := c.Client.Get(ctx, client.ObjectKey{Namespace: "training", Name: expected.Spec.ClaimRef.Name}, &claim); err != nil {
			t.Fatal(err)
		}
		if claim.Spec.VolumeName != expected.Name || *claim.Spec.StorageClassName != "shared" || claim.Labels[ExistingClaimRequestUIDLabel] != "request-uid" || claim.Labels["app"] != "trainer" {
			t.Fatalf("wrong claim: %+v", claim)
		}
		claim.Status.Phase = corev1.ClaimBound
		if err := c.Client.Status().Update(ctx, &claim); err != nil {
			t.Fatal(err)
		}
		var pv corev1.PersistentVolume
		if err := c.Client.Get(ctx, client.ObjectKey{Name: expected.Name}, &pv); err != nil {
			t.Fatal(err)
		}
		pv.Spec.ClaimRef.UID, pv.Spec.ClaimRef.ResourceVersion = claim.UID, claim.ResourceVersion
		if err := c.Client.Update(ctx, &pv); err != nil {
			t.Fatal(err)
		}
		pv.Status.Phase = corev1.VolumeBound
		if err := c.Client.Status().Update(ctx, &pv); err != nil {
			t.Fatal(err)
		}
	}
	if ready, err := f.ensure(c); !ready || err != nil {
		t.Fatalf("bound: ready=%v err=%v", ready, err)
	}
	if c.creates != len(f.pvs) || c.mutations != 0 {
		t.Fatalf("unexpected writes: %+v", c)
	}
	if !reflect.DeepEqual(before, []*unstructured.Unstructured{f.sts, f.migration, f.metadata, f.request}) {
		t.Fatal("management evidence mutated")
	}
}

func TestExistingTargetClaimsRejectBeforeCreate(t *testing.T) {
	cases := map[string]func(*existingClaimsFixture){
		"missing request UID":  func(f *existingClaimsFixture) { f.request.SetUID("") },
		"foreign workload UID": func(f *existingClaimsFixture) { f.sts.SetUID("other") },
		"foreign namespace":    func(f *existingClaimsFixture) { f.metadata.SetNamespace("other") },
		"foreign metadata UID workload": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedField(f.metadata.Object, "other", "spec", "workloadRef", "uid")
		},
		"migration request owner": func(f *existingClaimsFixture) { f.migration.SetAnnotations(nil) },
		"migration not complete": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedField(f.migration.Object, "Ready", "status", "phase")
		},
		"migration stale": func(f *existingClaimsFixture) { f.migration.SetGeneration(3) },
		"missing plan hash": func(f *existingClaimsFixture) {
			unstructured.RemoveNestedField(f.migration.Object, "status", "planHash")
		},
		"source not fenced": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedField(f.migration.Object, false, "spec", "sourceFenced")
		},
		"wrong migration target": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedField(f.migration.Object, "other", "spec", "targetCluster")
		},
		"same cluster": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedField(f.request.Object, "onprem", "spec", "targetCluster")
		},
		"foreign metadata ref": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedField(f.migration.Object, "other", "spec", "metadataRef")
		},
		"snapshot stale":                func(f *existingClaimsFixture) { f.snapshot()["observedGeneration"] = int64(2) },
		"snapshot missing collection":   func(f *existingClaimsFixture) { delete(f.snapshot(), "collectedAt") },
		"snapshot bad collection":       func(f *existingClaimsFixture) { f.snapshot()["collectedAt"] = "not-a-time" },
		"snapshot missing workload UID": func(f *existingClaimsFixture) { delete(f.snapshot(), "workloadUID") },
		"snapshot wrong cluster":        func(f *existingClaimsFixture) { f.snapshot()["clusterName"] = "other" },
		"snapshot duplicate": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedSlice(f.metadata.Object, []interface{}{f.snapshot(), f.snapshot()}, "status", "clusters")
		},
		"duplicate source volume": func(f *existingClaimsFixture) {
			f.snapshot()["volumes"] = []interface{}{f.sourceVolume(), f.sourceVolume()}
		},
		"foreign template":     func(f *existingClaimsFixture) { f.sourceVolume()["templateName"] = "foreign" },
		"wrong source ordinal": func(f *existingClaimsFixture) { f.sourceVolume()["ordinal"] = int64(1) },
		"wrong source PVC UID": func(f *existingClaimsFixture) { f.sourceVolume()["pvcUID"] = "recreated" },
		"source not NFS":       func(f *existingClaimsFixture) { delete(f.sourceVolume()["pvSpec"].(map[string]interface{}), "nfs") },
		"missing mappings":     func(f *existingClaimsFixture) { unstructured.RemoveNestedField(f.migration.Object, "spec", "volumes") },
		"duplicate mappings": func(f *existingClaimsFixture) {
			v, _, _ := unstructured.NestedSlice(f.migration.Object, "spec", "volumes")
			_ = unstructured.SetNestedSlice(f.migration.Object, []interface{}{v[0], v[0]}, "spec", "volumes")
		},
		"renamed mapping": func(f *existingClaimsFixture) {
			v, _, _ := unstructured.NestedSlice(f.migration.Object, "spec", "volumes")
			v[0].(map[string]interface{})["sourcePVC"] = "other"
			_ = unstructured.SetNestedSlice(f.migration.Object, v, "spec", "volumes")
		},
		"invalid pod ordinal": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedSlice(f.request.Object, []interface{}{map[string]interface{}{"sourcePod": "0"}}, "spec", "pods")
		},
		"work not applied":     func(f *existingClaimsFixture) { f.work()["applied"] = false },
		"work not detached":    func(f *existingClaimsFixture) { f.work()["detached"] = false },
		"work wrong namespace": func(f *existingClaimsFixture) { f.work()["namespace"] = "karmada-es-other" },
		"work wrong PV name":   func(f *existingClaimsFixture) { f.work()["name"] = "missing-pv" },
		"duplicate work": func(f *existingClaimsFixture) {
			_ = unstructured.SetNestedSlice(f.migration.Object, []interface{}{f.work(), f.work()}, "status", "works")
		},
		"PV foreign owner":           func(f *existingClaimsFixture) { f.pvs[1].Labels[existingClaimPVOwnerLabel] = "foreign" },
		"PV wrong manager":           func(f *existingClaimsFixture) { f.pvs[1].Labels[existingClaimPVManagedLabel] = "foreign" },
		"PV no UID":                  func(f *existingClaimsFixture) { f.pvs[1].UID = "" },
		"PV foreign claim namespace": func(f *existingClaimsFixture) { f.pvs[1].Spec.ClaimRef.Namespace = "foreign" },
		"PV foreign claim name":      func(f *existingClaimsFixture) { f.pvs[1].Spec.ClaimRef.Name = "foreign" },
		"PV already reserved UID":    func(f *existingClaimsFixture) { f.pvs[1].Spec.ClaimRef.UID = "old-claim" },
		"PV delete policy": func(f *existingClaimsFixture) {
			f.pvs[1].Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		},
		"PV different server": func(f *existingClaimsFixture) { f.pvs[1].Spec.NFS.Server = "foreign" },
		"PV different path":   func(f *existingClaimsFixture) { f.pvs[1].Spec.NFS.Path = "/foreign" },
		"PV readonly":         func(f *existingClaimsFixture) { f.pvs[1].Spec.NFS.ReadOnly = true },
		"PV different capacity": func(f *existingClaimsFixture) {
			f.pvs[1].Spec.Capacity[corev1.ResourceStorage] = resource.MustParse("3Gi")
		},
		"PV different class":     func(f *existingClaimsFixture) { f.pvs[1].Spec.StorageClassName = "foreign" },
		"PV released":            func(f *existingClaimsFixture) { f.pvs[1].Status.Phase = corev1.VolumeReleased },
		"foreign PVC":            func(f *existingClaimsFixture) { f.addClaim(t, 1, false); f.claims[0].Labels = nil },
		"wrong PVC spec":         func(f *existingClaimsFixture) { f.addClaim(t, 1, false); f.claims[0].Spec.VolumeName = "foreign" },
		"wrong PVC UID":          func(f *existingClaimsFixture) { f.addClaim(t, 1, true); f.claims[0].UID = "recreated" },
		"bound PVC no PV UID":    func(f *existingClaimsFixture) { f.addClaim(t, 1, true); f.pvs[1].Spec.ClaimRef.UID = "" },
		"bound PVC PV not bound": func(f *existingClaimsFixture) { f.addClaim(t, 1, true); f.pvs[1].Status.Phase = corev1.VolumeAvailable },
		"lost PVC":               func(f *existingClaimsFixture) { f.addClaim(t, 1, false); f.claims[0].Status.Phase = corev1.ClaimLost },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newExistingClaimsFixture(t)
			mutate(f)
			c := f.member(t)
			if ready, err := f.ensure(c); ready || err == nil {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			if c.creates != 0 || c.mutations != 0 {
				t.Fatalf("wrote before all evidence valid: creates=%d mutations=%d", c.creates, c.mutations)
			}
		})
	}
}

func TestExistingTargetClaimsMemberFailures(t *testing.T) {
	for _, kind := range []string{"pv-read", "pvc-read", "create-race"} {
		t.Run(kind, func(t *testing.T) {
			f := newExistingClaimsFixture(t)
			c := f.member(t)
			c.failPV, c.failPVC, c.failCreate = kind == "pv-read", kind == "pvc-read", kind == "create-race"
			if ready, err := f.ensure(c); ready || err == nil {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			if c.mutations != 0 || (kind != "create-race" && c.creates != 0) {
				t.Fatal("unexpected writes after member failure")
			}
		})
	}
}

func TestExistingTargetClaimsDefaultClassAndVolumeMode(t *testing.T) {
	f := newExistingClaimsFixture(t)
	templates, _, _ := unstructured.NestedSlice(f.sts.Object, "spec", "volumeClaimTemplates")
	for i, raw := range templates {
		delete(raw.(map[string]interface{})["spec"].(map[string]interface{}), "storageClassName")
		filesystem := corev1.PersistentVolumeFilesystem
		f.pvs[i].Spec.VolumeMode = &filesystem
	}
	_ = unstructured.SetNestedSlice(f.sts.Object, templates, "spec", "volumeClaimTemplates")
	c := f.member(t)
	if ready, err := f.ensure(c); ready || err != nil {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	for _, pv := range f.pvs {
		var claim corev1.PersistentVolumeClaim
		if err := c.Client.Get(context.Background(), client.ObjectKey{Namespace: "training", Name: pv.Spec.ClaimRef.Name}, &claim); err != nil {
			t.Fatal(err)
		}
		if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != pv.Spec.StorageClassName {
			t.Fatal("claim may dynamically provision instead of binding migrated PV")
		}
	}
}
