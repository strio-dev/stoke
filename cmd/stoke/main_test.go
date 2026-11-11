package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/strio-dev/stoke/internal/assets"
)

func TestCLI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		contains string
	}{
		{"help", []string{"--help"}, 0, "portable workflows"},
		{"version", []string{"version"}, 0, "stoke dev"},
		{"unknown command", []string{"unknown"}, 2, "unknown command"},
		{"run help", []string{"run", "--help"}, 0, "signature-policy"},
		{"publish missing destination", []string{"publish"}, 2, "requires one destination"},
		{"pull missing reference", []string{"pull"}, 2, "requires one OCI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(tc.args, &out, &errOut)
			if code != tc.code || !strings.Contains(out.String()+errOut.String(), tc.contains) {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
			}
			if tc.code != 0 && out.Len() != 0 {
				t.Fatal("failure wrote to stdout")
			}
		})
	}
}

func TestPullDirectoryOwnsOnlyFreshOutputs(t *testing.T) {
	parent := t.TempDir()
	destination := filepath.Join(parent, "package")
	data := []byte("version: 1\nassets: [scripts/check.sh]\ntasks: {check: {image: scratch}}\n")
	files := map[string]assets.File{"scripts/check.sh": {Data: []byte("#!/bin/sh\necho package\n"), Executable: true}}
	if err := pullDirectory(destination, data, files); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "stoke.yaml"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("workflow missing: %s %v", got, err)
	}
	info, err := os.Stat(filepath.Join(destination, "scripts/check.sh"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("asset mode %v %v", info, err)
	}
	before, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := pullDirectory(destination, []byte("replacement"), nil); err == nil {
		t.Fatal("existing directory accepted")
	}
	after, err := os.Stat(destination)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("existing directory replaced")
	}
	got, err = os.ReadFile(filepath.Join(destination, "stoke.yaml"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("existing workflow changed")
	}
	bad := filepath.Join(parent, "bad")
	if err := pullDirectory(bad, data, map[string]assets.File{"../escape": {Data: []byte("bad")}}); err == nil {
		t.Fatal("escape asset accepted")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "package" {
		t.Fatalf("partial output/staging survived: %v %v", entries, err)
	}
}

func TestPublishMissingAssetsHasNoRegistryWrites(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected registry access", 500)
	}))
	defer server.Close()
	dir := t.TempDir()
	file := filepath.Join(dir, "stoke.yaml")
	if err := os.WriteFile(file, []byte("version: 1\nassets: [missing.sh]\ntasks: {check: {image: scratch}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"publish", "--plain-http", "-f", file, strings.TrimPrefix(server.URL, "http://") + "/workflow:v1"}, &out, &errOut)
	if code != 1 || requests.Load() != 0 || out.Len() != 0 {
		t.Fatalf("invalid package published: code=%d requests=%d out=%q err=%q", code, requests.Load(), &out, &errOut)
	}
}

func TestCheckRejectsPackageStagingInsideContextOverride(t *testing.T) {
	module := t.TempDir()
	caller := t.TempDir()
	t.Setenv("TMPDIR", caller)
	if err := os.WriteFile(filepath.Join(module, "helper.sh"), []byte("component helper\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(module, "stoke.yaml")
	if err := os.WriteFile(file, []byte("version: 1\nassets: [helper.sh]\ntasks: {check: {image: scratch}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"check", "-f", file, "--context", caller}, &out, &errOut)
	if code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "TMPDIR") {
		t.Fatalf("overlap accepted: code=%d out=%q err=%q", code, &out, &errOut)
	}
	entries, err := os.ReadDir(caller)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned staging survived validation failure: %v %v", entries, err)
	}
}

func TestLocalWorkflowCLI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stoke.yaml")
	data := "version: 1\ntasks:\n  build:\n    image: scratch\n  test:\n    from: build\n    cache: false\n  artifact:\n    image: scratch\n    needs: [test]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"check", "-f", path}, &out, &errOut); code != 0 {
		t.Fatalf("check code %d: %s", code, &errOut)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"plan", "-f", path, "artifact"}, &out, &errOut); code != 0 {
		t.Fatalf("plan code %d: %s", code, &errOut)
	}
	if got := out.String(); got != "build\tcache=true\tneeds=\ntest\tcache=false\tneeds=build\nartifact\tcache=true\tneeds=test\n" {
		t.Fatalf("wrong task order/cache policy: %q", got)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"plan", "-f", path, "missing"}, &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatalf("unknown target code=%d out=%q err=%q", code, &out, &errOut)
	}
}

func TestMatrixPlanShowsConcreteJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stoke.yaml")
	data := "version: 1\ntasks:\n  test:\n    image: scratch\n    strategy:\n      matrix:\n        mode: [debug, release]\n  complete:\n    image: scratch\n    needs: [test]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"check", "-f", path}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "3 selected jobs") {
		t.Fatalf("check code=%d out=%q err=%q", code, &out, &errOut)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"plan", "-f", path, "complete"}, &out, &errOut); code != 0 {
		t.Fatalf("plan code=%d err=%q", code, &errOut)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `matrix={"mode":"debug"}`) || !strings.Contains(lines[1], `matrix={"mode":"release"}`) || !strings.HasPrefix(lines[2], "complete\t") {
		t.Fatalf("wrong matrix plan: %s", &out)
	}
}

func TestNativeCheckFilesAndPlan(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stoke.yaml")
	document := `version: 1
tasks:
  build: {coopr: {files: [image.coopr]}}
  publish:
    publish: {from: build, destinations: ['oci:/tmp/example-delivery']}
`
	if err := os.WriteFile(file, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"plan", "-f", file, "publish"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "kind=coopr-image") || !strings.Contains(out.String(), "kind=publish") {
		t.Fatalf("plan code=%d out=%q err=%q", code, &out, &errOut)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"check", "-f", file}, &out, &errOut); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "image.coopr") {
		t.Fatalf("missing input code=%d out=%q err=%q", code, &out, &errOut)
	}
	context := t.TempDir()
	// Host-file validation deliberately does not parse Coopr syntax or start an engine.
	if err := os.WriteFile(filepath.Join(context, "image.coopr"), []byte("not parsed by Stoke\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"check", "--context", context, "-f", file}, &out, &errOut); code != 0 {
		t.Fatalf("context check code=%d err=%q", code, &errOut)
	}
}
