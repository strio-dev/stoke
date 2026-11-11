package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckAndPlanInlineEnvironmentAndLocalMountAvailability(t *testing.T) {
	root := t.TempDir()
	definition := filepath.Join(root, "environment.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\" as=\"tools\" {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(root, "stoke.yaml")
	if err := os.WriteFile(workflow, []byte(`version: 1
tasks:
  test:
    environment: {files: [environment.coopr], target: tools}
    contexts: {data: ./missing}
    steps: [{run: [check], mounts: [{type: context, context: data, target: /input}]}]
`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if code := run([]string{"check", "-f", workflow}, &out, &diagnostics); code != 1 || !strings.Contains(diagnostics.String(), "unavailable") {
		t.Fatalf("missing named context: exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
	}
	if err := os.Mkdir(filepath.Join(root, "missing"), 0700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostics.Reset()
	if code := run([]string{"check", "-f", workflow}, &out, &diagnostics); code != 0 {
		t.Fatalf("valid environment: exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
	}
	out.Reset()
	diagnostics.Reset()
	if code := run([]string{"plan", "-f", workflow}, &out, &diagnostics); code != 0 || !strings.Contains(out.String(), "kind=environment") || !strings.Contains(out.String(), "target=tools") || !strings.Contains(out.String(), "mounts=1") {
		t.Fatalf("environment plan: exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
	}
	if err := os.Remove(definition); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostics.Reset()
	if code := run([]string{"check", "-f", workflow}, &out, &diagnostics); code != 1 || !strings.Contains(diagnostics.String(), "definition file") {
		t.Fatalf("missing environment file: exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
	}
}
