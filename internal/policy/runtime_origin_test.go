package policy

import "testing"

func TestRuntimeTemplateReceipt(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-uid", "stale", "missing-uid", "missing-generation", "invalid-member"} {
		t.Run(mode, func(t *testing.T) {
			o := NewObject("TrainingRuntime")
			o.SetUID("source-runtime")
			o.SetGeneration(2)
			s := map[string]interface{}{"observedGeneration": int64(1), "observedTemplateUID": "source-runtime", "observedTemplateGeneration": int64(2)}
			switch mode {
			case "wrong-uid":
				s["observedTemplateUID"] = "other"
			case "stale":
				s["observedTemplateGeneration"] = int64(1)
			case "missing-uid":
				delete(s, "observedTemplateUID")
			case "missing-generation":
				delete(s, "observedTemplateGeneration")
			case "invalid-member":
				s["observedGeneration"] = int64(0)
			}
			o.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": "aws1", "status": s}}}
			r := ReadRuntimeStatus(o, "aws1")
			if (r.ObservedGeneration == r.Generation) != (mode == "valid") {
				t.Fatalf("unexpected acceptance: %+v", r)
			}
		})
	}
}
