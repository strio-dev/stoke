package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCIGenerationSelectsStagesWithoutTheNativeCaller(t *testing.T) {
	t.Chdir(t.TempDir())
	const workflow = "version: 1\nci-template: github\ntasks:\n  build: {image: scratch}\n  test: {from: build, cache: false}\n  publish: {image: scratch, needs: [test]}\n"
	writeCIFile(t, "stoke.yaml", workflow)
	generate := func(args ...string) {
		t.Helper()
		if code, _, stderr := invokeCI(append([]string{"ci", "generate"}, args...)); code != 0 {
			t.Fatalf("generate %v: %s", args, stderr)
		}
	}
	generate("github", "test")
	original := readCIFile(t, stageAction("test"))
	for _, absent := range []string{".github", stageAction("build"), stageAction("publish")} {
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("single stage touched %s: %v", absent, err)
		}
	}
	const caller = ".github/workflows/manual.yml"
	const text = "# Handwritten and deliberately not a CI input\n"
	writeCIFile(t, caller, text)
	generate()
	generate("github", "--check")
	_ = readCIFile(t, stageAction("build"))
	_ = readCIFile(t, stageAction("publish"))
	if string(readCIFile(t, caller)) != text || string(readCIFile(t, "stoke.yaml")) != workflow ||
		!bytes.Equal(original, readCIFile(t, stageAction("test"))) {
		t.Fatal("generation modified authored inputs or changed the selected stage")
	}
	var action struct {
		Runs struct {
			Using string
			Steps []struct{ Run string }
		}
	}
	if err := yaml.Unmarshal(original, &action); err != nil || action.Runs.Using != "composite" || len(action.Runs.Steps) != 1 {
		t.Fatalf("invalid composite action: %#v %v", action, err)
	}
	script := action.Runs.Steps[0].Run
	for _, absent := range []string{"upload-artifact", "GITHUB_OUTPUT", "--needs", "actions/checkout"} {
		if strings.Contains(script, absent) {
			t.Fatalf("generated stage included %q", absent)
		}
	}
	if strings.TrimSpace(script) != "stoke run --ci test" {
		t.Fatalf("unexpected selected command: %q", script)
	}
}

func TestCIGenerationUsesAnExternalTemplateWithoutRegistration(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\nci-template: github\ntasks: {test: {image: scratch}}\n")
	writeCIFile(t, "jenkins.tmpl", "---\noutput: jenkins/{{.Job.ID}}.groovy\n---\nsh '''\n{{.Command}}\n'''\n")
	if code, _, stderr := invokeCI([]string{"ci", "generate", "test", "--template-file", "jenkins.tmpl"}); code != 0 {
		t.Fatal(stderr)
	}
	stage := readCIFile(t, ".stoke/ci/jenkins/test.groovy")
	if !bytes.HasPrefix(stage, []byte("sh '''\n")) || !bytes.Contains(stage, []byte("stoke run --ci test")) {
		t.Fatalf("unexpected external text: %s", stage)
	}
	if _, err := os.Stat(stageAction("test")); !os.IsNotExist(err) {
		t.Fatalf("root default overrode the explicit local file: %v", err)
	}
}

func TestCIGenerationProtectsExistingFilesAndPreflightsAllOutputs(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {first: {image: scratch}, test: {image: scratch}}\n")
	args := []string{"ci", "generate", "github"}
	if code, _, stderr := invokeCI(append(args, "--check")); code != 1 || !strings.Contains(stderr, "stale") {
		t.Fatalf("missing output passed check: %d %s", code, stderr)
	}
	if _, err := os.Stat(".stoke"); !os.IsNotExist(err) {
		t.Fatal("--check created directories")
	}
	path := stageAction("test")
	const handwritten = "my file\n"
	writeCIFile(t, path, handwritten)
	if code, _, stderr := invokeCI(args); code != 1 || !strings.Contains(stderr, "unmanaged") {
		t.Fatalf("unmanaged collision: %d %s", code, stderr)
	}
	if string(readCIFile(t, path)) != handwritten {
		t.Fatal("overwrote unmanaged file")
	}
	if _, err := os.Stat(stageAction("first")); !os.IsNotExist(err) {
		t.Fatal("a conflicting batch created other stages")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := invokeCI(args); code != 0 {
		t.Fatal(stderr)
	}
	writeCIFile(t, path, "edited generated stage")
	if code, _, stderr := invokeCI(args); code != 1 || !strings.Contains(stderr, "edited stage") {
		t.Fatalf("accepted edited stage: %d %s", code, stderr)
	}
	writeCIFile(t, "escape.tmpl", "---\noutput: ../../Jenkinsfile\n---\nsh 'true'\n")
	if code, _, stderr := invokeCI([]string{"ci", "generate", "--template-file", "escape.tmpl"}); code != 1 || !strings.Contains(stderr, "checkout-relative") {
		t.Fatalf("accepted escaping output: %d %s", code, stderr)
	}
}

func TestCIRunRequiresAnExplicitStage(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks: {test: {image: scratch}}\n")
	if code, _, stderr := invokeCI([]string{"run", "--ci"}); code != 1 || !strings.Contains(stderr, "explicit task") {
		t.Fatalf("CI without selection ran all tasks: %d %s", code, stderr)
	}
	if code, _, stderr := invokeCI([]string{"ci", "run"}); code != 2 || !strings.Contains(stderr, "unknown command") {
		t.Fatalf("legacy execution command survived: %d %s", code, stderr)
	}
}

func TestCIContextValidationUsesSelectedClosure(t *testing.T) {
	t.Chdir(t.TempDir())
	writeCIFile(t, "stoke.yaml", "version: 1\ntasks:\n  test: {image: scratch}\n  dev: {image: scratch, contexts: {local: missing-on-CI}}\n")
	for _, args := range [][]string{{"ci", "plan", "test"}, {"ci", "generate", "github", "test"}} {
		if code, _, stderr := invokeCI(args); code != 0 {
			t.Fatalf("unselected context blocked %v: exit=%d %s", args, code, stderr)
		}
	}
	if code, _, stderr := invokeCI([]string{"ci", "plan", "dev"}); code != 1 || !strings.Contains(stderr, "unavailable") {
		t.Fatalf("selected missing context accepted: exit=%d %s", code, stderr)
	}
}

func stageAction(name string) string { return ".stoke/ci/github/" + name + "/action.yml" }

func writeCIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readCIFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func invokeCI(args []string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := run(args, &out, &stderr)
	return code, out.String(), stderr.String()
}

func TestDefaultGroupsSelectDifferentDependencyClosures(t *testing.T) {
	t.Chdir(t.TempDir())
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(state, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(state, "cache"))
	definition := `version: 1
ci-template: github
groups:
  local: [lint]
  delivery: [publish]
default-run: local
default-ci: delivery
tasks:
  build:
    platform: linux/amd64
    host: {tools: [/bin/sh]}
    steps: [{run: [/bin/sh, -c, 'echo group-build']}]
  lint:
    platform: linux/amd64
    host: {tools: [/bin/sh]}
    steps: [{run: [/bin/sh, -c, 'echo group-lint']}]
  test:
    platform: linux/amd64
    needs: [build]
    host: {tools: [/bin/sh]}
    steps: [{run: [/bin/sh, -c, 'echo group-test']}]
  publish:
    platform: linux/amd64
    needs: [test]
    host: {tools: [/bin/sh]}
    steps: [{run: [/bin/sh, -c, 'echo group-publish']}]
`
	definition = strings.ReplaceAll(definition, "linux/amd64", runtime.GOOS+"/"+runtime.GOARCH)
	writeCIFile(t, "stoke.yaml", definition)
	if code, _, stderr := invokeCI([]string{"run", "--allow-host"}); code != 0 || !strings.Contains(stderr, "group-lint") || strings.Contains(stderr, "group-build") {
		t.Fatalf("wrong ordinary-run default: %d %s", code, stderr)
	}
	if code, _, stderr := invokeCI([]string{"run", "--allow-host", "delivery"}); code != 0 ||
		!strings.Contains(stderr, "group-build") || !strings.Contains(stderr, "group-test") ||
		!strings.Contains(stderr, "group-publish") || strings.Contains(stderr, "group-lint") {
		t.Fatalf("explicit group lost dependency closure: %d %s", code, stderr)
	}
	if code, _, stderr := invokeCI([]string{"ci", "generate"}); code != 0 {
		t.Fatal(stderr)
	}
	for _, stage := range []string{"build", "test", "publish"} {
		_ = readCIFile(t, stageAction(stage))
	}
	if _, err := os.Stat(stageAction("lint")); !os.IsNotExist(err) {
		t.Fatalf("generation used the run default: %v", err)
	}
	if code, _, stderr := invokeCI([]string{"ci", "generate", "github", "delivery", "--check"}); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := invokeCI([]string{"ci", "generate", "github"}); code != 0 {
		t.Fatal(stderr)
	}
	_ = readCIFile(t, stageAction("lint"))
	if string(readCIFile(t, "stoke.yaml")) != definition {
		t.Fatal("selection rewrote repository configuration")
	}
}
