package management

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Enumerate the claims actually used by this world. Claim templates override
// same-named pod volumes; a shared claim is migrated once, not once per rank.
func groupVolumeClaims(sts, request *unstructured.Unstructured) ([]string, error) {
	templates, _, err := unstructured.NestedSlice(sts.Object, "spec", "volumeClaimTemplates")
	if err != nil {
		return nil, err
	}
	pods, _, err := unstructured.NestedSlice(request.Object, "spec", "pods")
	if err != nil {
		return nil, err
	}
	claims := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			claims = append(claims, name)
		}
	}
	shadowed := map[string]bool{}
	for _, raw := range templates {
		template, ok := raw.(map[string]interface{})
		if !ok || stringField(template, "metadata", "name") == "" {
			return nil, fmt.Errorf("invalid volume claim template")
		}
		name := stringField(template, "metadata", "name")
		shadowed[name] = true
		if len(pods) == 0 {
			return nil, fmt.Errorf("missing restore pods for claim templates")
		}
		for _, rawPod := range pods {
			pod, ok := rawPod.(map[string]interface{})
			if !ok || stringField(pod, "sourcePod") == "" {
				return nil, fmt.Errorf("invalid restore pod")
			}
			add(name + "-" + stringField(pod, "sourcePod"))
		}
	}
	volumes, _, err := unstructured.NestedSlice(sts.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return nil, err
	}
	for _, raw := range volumes {
		volume, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid pod volume")
		}
		if shadowed[stringField(volume, "name")] {
			continue
		}
		claim, found, err := unstructured.NestedMap(volume, "persistentVolumeClaim")
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		name := stringField(claim, "claimName")
		if name == "" {
			return nil, fmt.Errorf("empty standalone PVC name")
		}
		add(name)
	}
	if injected := stringField(sts.Object, "spec", "template", "metadata", "annotations", "fluidcr.dcnlab.com/checkpoint-claim"); injected != "" {
		add(injected)
	}
	return claims, nil
}
