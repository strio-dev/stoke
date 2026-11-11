package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlatformPlanResolvesMatrixAndInputWithoutEngine(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stoke.yaml")
	data := `version: 1
inputs:
  target: {default: linux/aarch64}
tasks:
  matrix:
    image: scratch
    platform: {matrix: target}
    strategy: {matrix: {target: [linux/amd64, linux/arm64]}}
  input:
    image: scratch
    platform: {input: target}
`
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"plan", "-f", file}, &out, &errOut); code != 0 {
		t.Fatalf("plan: %d %s", code, &errOut)
	}
	if strings.Count(out.String(), "platform=linux/arm64") != 2 || strings.Count(out.String(), "platform=linux/amd64") != 1 || strings.Contains(out.String(), "linux/aarch64") {
		t.Fatalf("unresolved platform plan: %s", &out)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"check", "-f", file}, &out, &errOut); code != 0 {
		t.Fatalf("check: %d %s", code, &errOut)
	}
}

func TestInvalidPlatformFailsBeforeEngineStartup(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stoke.yaml")
	if err := os.WriteFile(file, []byte("version: 1\ntasks:\n  test: {image: scratch, platform: windows/amd64}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "-f", file, "--coopr-binary", "/not/an/engine"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "platform") || strings.Contains(errOut.String(), "fork/exec") || out.Len() != 0 {
		t.Fatalf("invalid platform reached engine: %d %s %s", code, &out, &errOut)
	}
}
