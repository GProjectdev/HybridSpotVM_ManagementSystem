package main

import "testing"

func TestCommandFlagsDoNotConflictWithControllerRuntime(t *testing.T) {
	for i := 0; i < 2; i++ {
		o, err := parseOptions([]string{"--kubeconfig=/readonly/config", "--namespace=ns", "--run=a", "--run-uid=uid", "--workload-uid=work"})
		if err != nil || o.path != "/readonly/config" || o.uid != "uid" || o.workUID != "work" {
			t.Fatalf("command did not parse: %+v %v", o, err)
		}
	}
	if _, err := parseOptions([]string{"--unexpected=value"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}
