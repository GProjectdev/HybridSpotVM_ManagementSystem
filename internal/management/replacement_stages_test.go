package management

import (
	"context"
	"reflect"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Existing workflow tests exercise both independently registered stages. A VM
// validation failure still lets the coordinator record the rejection reason.
type replacementStagesFixture struct{ *ReplacementReconciler }

func (r *replacementStagesFixture) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	vm := &ReplacementCapacityReconciler{Client: r.Client, APIReader: r.APIReader, Clock: r.Clock}
	_, _ = vm.Reconcile(ctx, req)
	return r.ReplacementReconciler.Reconcile(ctx, req)
}

func TestReplacementStagesHaveExclusiveWrites(t *testing.T) {
	ctx := context.Background()
	op := replacementOperationFixture()
	fixture := replacementReconcilerFixture(t, time.Now(), op, replacementPolicyFixture(), replacementOldNodeProvisionFixture())
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}
	coordinator := &ReplacementReconciler{Client: fixture.Client, APIReader: fixture.Client, Clock: fixture.Clock}
	if _, err := coordinator.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	node := p.NewObject("NodeProvision")
	if err := fixture.Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: "new"}, node); !apierrors.IsNotFound(err) {
		t.Fatalf("coordinator created capacity: %v", err)
	}
	if err := fixture.Get(ctx, request.NamespacedName, op); err != nil {
		t.Fatal(err)
	}
	status := op.DeepCopy().Object["status"]
	vm := &ReplacementCapacityReconciler{Client: fixture.Client, APIReader: fixture.Client, Clock: fixture.Clock}
	if _, err := vm.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Get(ctx, request.NamespacedName, op); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status, op.Object["status"]) {
		t.Fatal("VM stage mutated coordinator status")
	}
	if err := fixture.Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: "new"}, node); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{newRestoreRequest(), p.NewObject("SpotRecovery"), p.NewObject("FluidCRMigration")} {
		if err := fixture.Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: op.GetName() + "-restore"}, obj); !apierrors.IsNotFound(err) {
			t.Fatalf("VM stage produced %s: %v", obj.GetObjectKind().GroupVersionKind().Kind, err)
		}
	}
}
