package member

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func metadataServer(t *testing.T, action *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/api/token" {
			if r.Method != "PUT" {
				t.Error("token must use PUT")
			}
			w.Write([]byte("token"))
			return
		}
		if r.Header.Get("X-aws-ec2-metadata-token") != "token" {
			t.Error("missing IMDSv2 token")
		}
		switch r.URL.Path {
		case "/latest/meta-data/instance-id":
			w.Write([]byte("i-123"))
		case "/latest/meta-data/spot/instance-action":
			if *action == "" {
				w.WriteHeader(404)
			} else {
				w.Write([]byte(*action))
			}
		default:
			w.WriteHeader(404)
		}
	}))
}
func TestSpotNoticeLatchedAndProvisioningPreserved(t *testing.T) {
	action := `{"action":"terminate","time":"2026-09-26T12:00:00Z"}`
	server := metadataServer(t, &action)
	defer server.Close()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	np := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "ml.dcn.ssu.ac.kr/v1alpha1", "kind": "NodeProvision", "metadata": map[string]interface{}{"name": "worker", "namespace": "infra", "uid": "owner"}, "status": map[string]interface{}{"phase": "Ready", "instanceId": "i-123"}}}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "ml.dcn.ssu.ac.kr", Version: "v1alpha1", Kind: "NodeProvision"}, &unstructured.Unstructured{})
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{"ml.dcn.ssu.ac.kr/node-provision": "worker", "ml.dcn.ssu.ac.kr/node-provision-namespace": "infra", "ml.dcn.ssu.ac.kr/node-provision-uid": "owner"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(np).WithObjects(np, node).Build()
	watcher := &SpotWatcher{Client: c, NodeName: "node", Metadata: &MetadataClient{HTTP: server.Client(), BaseURL: server.URL}}
	if err := watcher.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	action = ""
	if err := watcher.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(np), np); err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(np.Object, "status", "phase")
	risk, _, _ := unstructured.NestedBool(np.Object, "status", "spot", "atRisk")
	if phase != "Ready" || !risk {
		t.Fatalf("status lost: %#v", np.Object["status"])
	}
	node.Labels["ml.dcn.ssu.ac.kr/node-provision-uid"] = "recreated"
	if err := c.Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Observe(context.Background()); err == nil {
		t.Fatal("accepted recreated owner")
	}
}
func TestInvalidInterruptionDoesNotBecomeRiskEvidence(t *testing.T) {
	action := `{"action":"terminate","time":"not-a-time"}`
	server := metadataServer(t, &action)
	defer server.Close()
	md := &MetadataClient{HTTP: server.Client(), BaseURL: server.URL}
	if _, err := md.Poll(context.Background()); err == nil {
		t.Fatal("invalid timestamp accepted")
	}
}
