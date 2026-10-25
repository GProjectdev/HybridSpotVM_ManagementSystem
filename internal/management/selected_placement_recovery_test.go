package management

import (
	"context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func selectedRecoveryFixture() (*unstructured.Unstructured, *unstructured.Unstructured, []*unstructured.Unstructured) {
	old, policy, objects := applicationFailoverFixture()
	name := groupPlacementRequestName(policy.GetName(), string(old.GetUID()), "aws")
	objects[1].SetName(name)
	owners := objects[2].GetOwnerReferences()
	owners[0].Name = name
	objects[2].SetOwnerReferences(owners)
	annotations := old.GetAnnotations()
	annotations[placementRequestAnnotation] = name
	old.SetAnnotations(annotations)
	objects[4].SetAnnotations(map[string]string{placementRequestUIDAnnotation: string(objects[1].GetUID())})
	reports, _, _ := unstructured.NestedSlice(objects[2].Object, "status", "clusters")
	source := objects[2].DeepCopy()
	sourceReports, _, _ := unstructured.NestedSlice(source.Object, "status", "clusters")
	sourceReport := sourceReports[0].(map[string]interface{})
	sourceReport["clusterName"] = "onprem"
	sourceReport["phase"] = "SourceFenced"
	_ = unstructured.SetNestedSlice(objects[2].Object, append(reports, sourceReport), "status", "clusters")
	old = applicationFailoverRemoval(old)
	_ = unstructured.SetNestedSlice(old.Object, []interface{}{map[string]interface{}{"name": "aws", "replicas": int64(1)}}, "spec", "clusters")
	a := old.GetAnnotations()
	a[recoverSelectedPlacement] = string(old.GetUID())
	old.SetAnnotations(a)
	return old, policy, objects
}

func TestSelectedRecoveryPreservesHoldAndRestoresIntent(t *testing.T) {
	old, policy, objects := selectedRecoveryFixture()
	h := applicationFailoverHandler(objects)
	next, err := recoverSelectedBinding(context.Background(), h.Reader, policy, old)
	if err != nil {
		t.Fatal(err)
	}
	if !boolField(next.Object, "spec", "suspension", "dispatching") || next.GetAnnotations()[groupVolumeHold] != old.GetAnnotations()[groupVolumeHold] {
		t.Fatal("recovery released dispatch")
	}
	clusters, _ := applicationFailoverClusters(next)
	if len(clusters) != 0 || next.GetAnnotations()[pendingPlacementAnnotation] == "" || next.GetAnnotations()[recoverSelectedPlacement] != "" {
		t.Fatal("scheduling intent was not reconstructed")
	}
	applicationFailoverAdmit(t, h, old, next, applicationFailoverTestUser, "")
	applicationFailoverAdmit(t, h, old, next, "untrusted", "only the placement controller")
	unsafe := next.DeepCopy()
	_ = unstructured.SetNestedField(unsafe.Object, false, "spec", "suspension", "dispatching")
	applicationFailoverAdmit(t, h, old, unsafe, applicationFailoverTestUser, "must preserve")
}

func TestSelectedRecoveryRejectsMissingOrStaleEvidence(t *testing.T) {
	tests := map[string]func(*unstructured.Unstructured, []*unstructured.Unstructured){
		"missing opt-in": func(b *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			a := b.GetAnnotations()
			delete(a, recoverSelectedPlacement)
			b.SetAnnotations(a)
		},
		"stale opt-in": func(b *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			a := b.GetAnnotations()
			a[recoverSelectedPlacement] = "old"
			b.SetAnnotations(a)
		},
		"foreign hold": func(b *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			a := b.GetAnnotations()
			a[groupVolumeHold] = "other"
			b.SetAnnotations(a)
		},
		"no suspension": func(b *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			unstructured.RemoveNestedField(b.Object, "spec", "suspension")
		},
		"stale request": func(b *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			a := b.GetAnnotations()
			a[placementRequestUIDAnnotation] = "old"
			a[groupVolumeHold] = "old"
			b.SetAnnotations(a)
		},
		"missing fence": func(_ *unstructured.Unstructured, o []*unstructured.Unstructured) {
			unstructured.RemoveNestedField(o[2].Object, "status")
		},
		"PV incomplete": func(_ *unstructured.Unstructured, o []*unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o[4].Object, "Preparing", "status", "phase")
		},
		"wrong target": func(b *unstructured.Unstructured, _ []*unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(b.Object, []interface{}{map[string]interface{}{"name": "other"}}, "spec", "clusters")
		},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			old, policy, objects := selectedRecoveryFixture()
			edit(old, objects)
			h := applicationFailoverHandler(objects)
			if _, err := recoverSelectedBinding(context.Background(), h.Reader, policy, old); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
		})
	}
}

func TestRecoveredPlacementStillRequiresPreparedBeforeRelease(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		old, policy, objects := selectedRecoveryFixture()
		if !prepared {
			_ = unstructured.SetNestedField(objects[1].Object, "Preparing", "status", "phase")
			targetReport(objects[2])["phase"] = "Preparing"
		}
		h := applicationFailoverHandler(objects)
		held, err := recoverSelectedBinding(context.Background(), h.Reader, policy, old)
		if err != nil {
			t.Fatal(err)
		}
		next := held.DeepCopy()
		a := next.GetAnnotations()
		delete(a, pendingPlacementAnnotation)
		delete(a, applicationFailoverSourceAnnotation)
		delete(a, applicationFailoverTrigger)
		delete(a, groupVolumeHold)
		next.SetAnnotations(a)
		unstructured.RemoveNestedField(next.Object, "spec", "suspension", "dispatching")
		_ = unstructured.SetNestedSlice(next.Object, []interface{}{map[string]interface{}{"name": "aws", "replicas": int64(1)}}, "spec", "clusters")
		if !prepared {
			handled, err := h.holdApplicationFailover(context.Background(), policy, held, next, applicationFailoverTestUser)
			if err == nil {
				t.Fatalf("unprepared release accepted: handled=%v", handled)
			}
		} else {
			applicationFailoverAdmit(t, h, held, next, applicationFailoverTestUser, "")
		}
	}
}

func TestSelectedRecoveryRejectsForeignMigrationAndRequestName(t *testing.T) {
	for _, foreignMigration := range []bool{false, true} {
		old, policy, objects := selectedRecoveryFixture()
		if foreignMigration {
			objects[4].SetAnnotations(map[string]string{placementRequestUIDAnnotation: "another-request"})
		} else {
			a := old.GetAnnotations()
			a[placementRequestAnnotation] = "different-name"
			old.SetAnnotations(a)
			objects[1].SetName("different-name")
			owners := objects[2].GetOwnerReferences()
			owners[0].Name = "different-name"
			objects[2].SetOwnerReferences(owners)
		}
		h := applicationFailoverHandler(objects)
		if _, err := recoverSelectedBinding(context.Background(), h.Reader, policy, old); err == nil {
			t.Fatal("accepted recovery that cannot continue with the same operation")
		}
	}
}
