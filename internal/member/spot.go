package member

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const metadataURL = "http://169.254.169.254"

var metadataHTTP = &http.Client{
	Timeout:       2 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
}

type SpotSignal struct {
	Type             string
	NoticeTime       string
	InterruptionTime string
	Action           string
	InstanceID       string
}

type MetadataClient struct {
	HTTP    *http.Client
	BaseURL string
}

func (m *MetadataClient) request(ctx context.Context, method, path, token string) ([]byte, int, error) {
	base := m.BaseURL
	if base == "" {
		base = metadataURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("X-aws-ec2-metadata-token", token)
	} else {
		req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	}
	h := m.HTTP
	if h == nil {
		h = metadataHTTP
	}
	resp, err := h.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if len(b) > 16384 {
		return nil, resp.StatusCode, fmt.Errorf("metadata response too large")
	}
	return b, resp.StatusCode, err
}

func (m *MetadataClient) Poll(ctx context.Context) (SpotSignal, error) {
	var signal SpotSignal
	token, code, err := m.request(ctx, http.MethodPut, "/latest/api/token", "")
	if err != nil {
		return signal, err
	}
	if code != 200 {
		return signal, fmt.Errorf("IMDSv2 token: HTTP %d", code)
	}
	tok := strings.TrimSpace(string(token))
	id, code, err := m.request(ctx, http.MethodGet, "/latest/meta-data/instance-id", tok)
	if err != nil {
		return signal, err
	}
	if code != 200 {
		return signal, fmt.Errorf("instance identity: HTTP %d", code)
	}
	signal.InstanceID = strings.TrimSpace(string(id))
	b, code, err := m.request(ctx, http.MethodGet, "/latest/meta-data/spot/instance-action", tok)
	if err != nil {
		return signal, err
	}
	if code == 200 {
		var event struct {
			Action string `json:"action"`
			Time   string `json:"time"`
		}
		if err = json.Unmarshal(b, &event); err != nil {
			return signal, err
		}
		if event.Action != "terminate" && event.Action != "stop" && event.Action != "hibernate" {
			return signal, fmt.Errorf("unknown interruption action")
		}
		if _, err = time.Parse(time.RFC3339, event.Time); err != nil {
			return signal, fmt.Errorf("invalid interruption time: %w", err)
		}
		signal.Type = "InterruptionNotice"
		signal.Action = event.Action
		signal.InterruptionTime = event.Time
		return signal, nil
	}
	if code != 404 {
		return signal, fmt.Errorf("interruption metadata: HTTP %d", code)
	}
	b, code, err = m.request(ctx, http.MethodGet, "/latest/meta-data/events/recommendations/rebalance", tok)
	if err != nil {
		return signal, err
	}
	if code == 404 {
		return signal, nil
	}
	if code != 200 {
		return signal, fmt.Errorf("rebalance metadata: HTTP %d", code)
	}
	var event struct {
		NoticeTime string `json:"noticeTime"`
	}
	if err = json.Unmarshal(b, &event); err != nil {
		return signal, err
	}
	if _, err = time.Parse(time.RFC3339, event.NoticeTime); err != nil {
		return signal, err
	}
	signal.Type = "RebalanceRecommendation"
	signal.NoticeTime = event.NoticeTime
	return signal, nil
}

type SpotWatcher struct {
	Client   client.Client
	NodeName string
	Metadata *MetadataClient
}

func (*SpotWatcher) NeedLeaderElection() bool { return false }
func (w *SpotWatcher) Start(ctx context.Context) error {
	if w.NodeName == "" {
		return fmt.Errorf("NODE_NAME required")
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.Observe(ctx); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "Spot observation failed", "node", w.NodeName)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (w *SpotWatcher) Observe(ctx context.Context) error {
	node := &corev1.Node{}
	if err := w.Client.Get(ctx, client.ObjectKey{Name: w.NodeName}, node); err != nil {
		return err
	}
	name, ns, uid := node.Labels["ml.dcn.ssu.ac.kr/node-provision"], node.Labels["ml.dcn.ssu.ac.kr/node-provision-namespace"], node.Labels["ml.dcn.ssu.ac.kr/node-provision-uid"]
	if name == "" || ns == "" || uid == "" {
		return fmt.Errorf("node lacks NodeProvision ownership labels")
	}
	md := w.Metadata
	if md == nil {
		md = &MetadataClient{}
	}
	signal, err := md.Poll(ctx)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		np := &unstructured.Unstructured{}
		np.SetGroupVersionKind(schema.GroupVersionKind{Group: "ml.dcn.ssu.ac.kr", Version: "v1alpha1", Kind: "NodeProvision"})
		if err := w.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, np); err != nil {
			return err
		}
		if string(np.GetUID()) != uid {
			return fmt.Errorf("NodeProvision UID mismatch")
		}
		instance, _, _ := unstructured.NestedString(np.Object, "status", "instanceId")
		if instance == "" || instance != signal.InstanceID {
			return fmt.Errorf("instance identity mismatch")
		}
		base := np.DeepCopy()
		spot, _, _ := unstructured.NestedMap(np.Object, "status", "spot")
		if spot == nil {
			spot = map[string]interface{}{}
		}
		now := time.Now().UTC().Format(time.RFC3339)
		spot["lastHeartbeatTime"] = now
		spot["instanceID"] = instance
		// Notices remain latched: metadata disappearing is not proof of recovery.
		oldType, _ := spot["signalType"].(string)
		if signal.Type != "" && !(oldType == "InterruptionNotice" && signal.Type == "RebalanceRecommendation") {
			digest := sha256.Sum256([]byte(instance + "|" + signal.Type + "|" + signal.NoticeTime + "|" + signal.InterruptionTime + "|" + signal.Action))
			eventID := hex.EncodeToString(digest[:])
			if spot["eventID"] != eventID {
				spot["noticeTime"] = now
				if signal.NoticeTime != "" {
					spot["noticeTime"] = signal.NoticeTime
				}
			}
			spot["atRisk"] = true
			spot["signalType"] = signal.Type
			spot["eventID"] = eventID
			if signal.InterruptionTime != "" {
				spot["interruptionTime"] = signal.InterruptionTime
			}
			if signal.Action != "" {
				spot["action"] = signal.Action
			}
		}
		if err := unstructured.SetNestedMap(np.Object, spot, "status", "spot"); err != nil {
			return err
		}
		return w.Client.Status().Patch(ctx, np, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}
