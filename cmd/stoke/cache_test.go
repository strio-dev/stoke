package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRegistryCacheFlagsValidateBeforeLoadingWorkflow(t *testing.T) {
	for _, flag := range []string{"--cache-from", "--cache-to"} {
		for _, value := range []string{"", "team/cache", "registry.example/cache:tag", "registry.example/cache:", "https://registry.example/cache", "oci:/tmp/cache", "type=local,src=/cache", "type=registry,ref=registry.example/cache"} {
			t.Run(flag+value, func(t *testing.T) {
				var out, errOut bytes.Buffer
				code := run([]string{"run", flag, value, "-f", "/missing/workflow.yaml", "--coopr-binary", "/missing/coopr"}, &out, &errOut)
				if code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), strings.TrimPrefix(flag, "--")) || strings.Contains(errOut.String(), "no such file") {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &errOut)
				}
			})
		}
	}
}

func TestRegistryCacheFlagsAreRepeatableRunOptions(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--cache-from", "registry.example/one", "--cache-from", "registry.example/two", "--cache-to", "registry.example/write-one", "--cache-to", "registry.example/write-two", "-f", "/missing/workflow.yaml"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "no such file") {
		t.Fatalf("repeat flags were not accepted: code=%d stdout=%s stderr=%s", code, &out, &errOut)
	}
	for _, command := range []string{"check", "plan", "publish", "pull"} {
		out.Reset()
		errOut.Reset()
		if code := run([]string{command, "--cache-from", "registry.example/read"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "flag provided but not defined") {
			t.Fatalf("%s accepted a run cache option: code=%d stderr=%s", command, code, &errOut)
		}
	}
}
