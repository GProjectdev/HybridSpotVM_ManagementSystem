package member

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestRuntimeOriginSeparatesAPIServerGenerations(t *testing.T) {
	for _, mode := range []string{"valid", "uid-missing", "generation-missing", "invalid", "zero", "local"} {
		t.Run(mode, func(t *testing.T) {
			o := &unstructured.Unstructured{}
			o.SetGeneration(1)
			a := map[string]string{"resourcetemplate.karmada.io/uid": "source-runtime", "resourcetemplate.karmada.io/generation": "2"}
			switch mode {
			case "uid-missing":
				delete(a, "resourcetemplate.karmada.io/uid")
			case "generation-missing":
				delete(a, "resourcetemplate.karmada.io/generation")
			case "invalid":
				a["resourcetemplate.karmada.io/generation"] = "bad"
			case "zero":
				a["resourcetemplate.karmada.io/generation"] = "0"
			case "local":
				a = nil
			}
			o.SetAnnotations(a)
			status := map[string]interface{}{"observedGeneration": int64(1)}
			err := recordRuntimeOrigin(o, status)
			if (err == nil) != (mode == "valid" || mode == "local") {
				t.Fatalf("unexpected error %v", err)
			}
			if status["observedGeneration"] != int64(1) {
				t.Fatal("member generation overwritten")
			}
			if mode == "valid" && (status["observedTemplateUID"] != "source-runtime" || status["observedTemplateGeneration"] != int64(2)) {
				t.Fatalf("missing origin receipt: %v", status)
			}
		})
	}
}
