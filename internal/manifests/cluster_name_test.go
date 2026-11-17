package manifests_test

import (
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	schema "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"os"
	"sigs.k8s.io/yaml"
	"testing"
)

func TestOperationCRDsAcceptRegisteredClusterNames(t *testing.T) {
	for _, name := range []string{"spotreplacements", "spotrecoveries"} {
		b, e := os.ReadFile("../../config/crd/" + name + ".yaml")
		if e != nil {
			t.Fatal(e)
		}
		var crd v1.CustomResourceDefinition
		if e = yaml.Unmarshal(b, &crd); e != nil {
			t.Fatal(e)
		}
		fields := []string{"sourceCluster"}
		if name == "spotreplacements" {
			fields = append(fields, "targetCluster")
		}
		for _, f := range fields {
			prop := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties[f]
			internal := &ext.JSONSchemaProps{}
			if e = v1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&prop, internal, nil); e != nil {
				t.Fatal(e)
			}
			validator, _, e := schema.NewSchemaValidator(internal)
			if e != nil {
				t.Fatal(e)
			}
			for _, cluster := range []string{"aws", "aws1", "aws-gpu-2", "", "AWS_1", "bad/name"} {
				valid := cluster == "aws" || cluster == "aws1" || cluster == "aws-gpu-2"
				errs := schema.ValidateCustomResource(field.NewPath("spec", f), cluster, validator)
				if (len(errs) == 0) != valid {
					t.Fatalf("%s %s=%q errors=%v", name, f, cluster, errs)
				}
			}
		}
	}
}
