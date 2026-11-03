package management

import (
	"context"
	"reflect"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGroupStandaloneClaims(t *testing.T) {
	request, _ := groupReleaseFixture(true)
	sts := p.NewObject("StatefulSet")
	sts.SetNamespace("demo")
	sts.SetName("trainer")
	sts.SetUID("world")
	sts.Object["spec"] = map[string]interface{}{
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{"annotations": map[string]interface{}{"fluidcr.dcnlab.com/checkpoint-claim": "shared"}},
			"spec": map[string]interface{}{"volumes": []interface{}{
				map[string]interface{}{"name": "data", "persistentVolumeClaim": map[string]interface{}{"claimName": "dataset"}},
				map[string]interface{}{"name": "checkpoint", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}},
				map[string]interface{}{"name": "alias", "persistentVolumeClaim": map[string]interface{}{"claimName": "shared"}},
				map[string]interface{}{"name": "script", "configMap": map[string]interface{}{"name": "script"}},
			}},
		},
	}
	claims, err := groupVolumeClaims(sts, request)
	if err != nil || !reflect.DeepEqual(claims, []string{"dataset", "shared"}) {
		t.Fatalf("claims=%v err=%v", claims, err)
	}
	// Shared-only worlds must no longer bypass migration receipt checks.
	if err := validateGroupVolumes(context.Background(), fake.NewClientBuilder().WithObjects(sts).Build(), placementBinding("onprem"), request); err == nil {
		t.Fatal("standalone claims released without a migration")
	}
	_ = unstructured.SetNestedSlice(sts.Object, []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data"}}}, "spec", "volumeClaimTemplates")
	claims, err = groupVolumeClaims(sts, request)
	if err != nil || !reflect.DeepEqual(claims, []string{"data-trainer-0", "shared"}) {
		t.Fatalf("shadow claims=%v err=%v", claims, err)
	}
	_ = unstructured.SetNestedSlice(sts.Object, []interface{}{map[string]interface{}{"name": "bad", "persistentVolumeClaim": map[string]interface{}{}}}, "spec", "template", "spec", "volumes")
	if _, err := groupVolumeClaims(sts, request); err == nil {
		t.Fatal("accepted empty claimName")
	}
}
