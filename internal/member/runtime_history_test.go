package member

import "testing"

func TestRuntimeHistoryRequiresSameIdentityAndChronologicalSamples(t *testing.T) {
	old := map[string]interface{}{"name": "train-0", "uid": "pod-1", "rank": int64(0), "checkpointID": "round-1", "globalStep": int64(10), "observedAt": "2026-09-26T00:00:00Z"}
	newSample := func() map[string]interface{} {
		return map[string]interface{}{"name": "train-0", "uid": "pod-1", "rank": int64(0), "checkpointID": "round-1", "globalStep": int64(12), "observedAt": "2026-09-26T00:00:05Z"}
	}
	current := newSample()
	attachPreviousObservations([]interface{}{current}, []interface{}{old})
	if current["previousGlobalStep"] != int64(10) || current["previousObservedAt"] != old["observedAt"] {
		t.Fatalf("missing history: %v", current)
	}
	for _, change := range []struct {
		key   string
		value interface{}
	}{
		{"uid", "pod-2"}, {"rank", int64(1)}, {"checkpointID", "round-2"}, {"globalStep", int64(9)}, {"observedAt", "2026-09-25T23:59:59Z"},
	} {
		p := newSample()
		p[change.key] = change.value
		attachPreviousObservations([]interface{}{p}, []interface{}{old})
		if _, ok := p["previousGlobalStep"]; ok {
			t.Fatalf("reused history after %s changed: %v", change.key, p)
		}
	}
	unchanged := newSample()
	attachPreviousObservations([]interface{}{unchanged}, []interface{}{current})
	if unchanged["previousGlobalStep"] != int64(10) {
		t.Fatal("same sample should retain original observation, not manufacture progress")
	}
}
