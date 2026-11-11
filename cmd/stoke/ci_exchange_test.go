package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/ci"
	"github.com/strio-dev/stoke/internal/runner"
	"github.com/strio-dev/stoke/internal/workflow"
)

func testCIScope(plan string) artifact.CIScope {
	return artifact.CIScope{Provider: "github", Server: "https://github.com", Repository: "fixture/stoke", Run: "7", Source: strings.Repeat("f", 40), Invocation: strings.Repeat("a", 32), Plan: plan}
}

func TestCIScopeSeparatesRepeatedReusableCalls(t *testing.T) {
	for name, value := range map[string]string{"GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "fixture/stoke", "GITHUB_RUN_ID": "9", "GITHUB_SHA": strings.Repeat("a", 40)} {
		t.Setenv(name, value)
	}
	file := filepath.Join(t.TempDir(), "outputs")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if err := createCIScope(strings.Repeat("b", 64), file, &first); err != nil {
		t.Fatal(err)
	}
	if err := createCIScope(strings.Repeat("b", 64), "", &second); err != nil {
		t.Fatal(err)
	}
	var a, b artifact.CIScope
	if err := json.Unmarshal(first.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.Invocation == b.Invocation || a.Run != b.Run || a.Source != b.Source {
		t.Fatal("repeated calls shared invocation identity or lost native context")
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "scope="+strings.TrimSpace(first.String())+"\n" {
		t.Fatalf("native scope output: %s %v", data, err)
	}
}

func TestNativeOutputFileRejectsSymlinksAndMultiline(t *testing.T) {
	file := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := file + "-link"
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := writeGitHubOutput("key", "value", link); err == nil {
		t.Fatal("followed native-output symlink")
	}
	if err := writeGitHubOutput("key", "value\ninjected=bad", file); err == nil {
		t.Fatal("allowed extra native output lines")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "original" {
		t.Fatal("failed output validation changed file")
	}
}

func nativePlan(t *testing.T) (*workflow.Workflow, *ci.Plan, ci.Job, ci.Job) {
	t.Helper()
	w, err := workflow.Decode(strings.NewReader("version: 1\ntasks:\n  build: {image: scratch}\n  test: {from: build, cache: false}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ci.Build(w, ci.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return w, p, p.Groups[0].Jobs[0], p.Groups[1].Jobs[0]
}

func TestNativeMetadataAllowsExplicitPriorAttemptButRejectsMissingFutureAndWrongScope(t *testing.T) {
	_, p, build, check := nativePlan(t)
	scopeHash, _ := testCIScope(p.ID).Hash()
	metadata := ciNativeMetadata{Version: 1, Scope: scopeHash, Unit: build.ID, Attempt: 1, Inputs: "registry.example.com/results@sha256:" + strings.Repeat("1", 64)}
	data, _ := json.Marshal(metadata)
	t.Setenv(ci.InputEnv(build.OutputKey), string(data))
	options := ciExchangeOptions{Attempt: 2, NeedsEnv: true}
	if refs, err := readCINativeInputs(p, check, options, scopeHash); err != nil || len(refs) != 1 {
		t.Fatalf("explicit prior-attempt selection rejected: %v %v", refs, err)
	}
	for _, scenario := range []string{"missing", "scope", "future", "unit"} {
		t.Run(scenario, func(t *testing.T) {
			v := metadata
			switch scenario {
			case "scope":
				v.Scope = strings.Repeat("0", 64)
			case "future":
				v.Attempt = 3
			case "unit":
				v.Unit = "another"
			}
			value, _ := json.Marshal(v)
			if scenario == "missing" {
				value = nil
			}
			t.Setenv(ci.InputEnv(build.OutputKey), string(value))
			if _, err := readCINativeInputs(p, check, options, scopeHash); err == nil {
				t.Fatal("invalid native selection accepted")
			}
		})
	}
}

func TestNoResultRegistryFailsBeforeAnyEngineOrStaging(t *testing.T) {
	w, p, _, check := nativePlan(t)
	scope := testCIScope(p.ID)
	data, _ := json.Marshal(scope)
	cache := filepath.Join(t.TempDir(), "not-created")
	_, err := runCIExchange(context.Background(), w, p, check, runner.Options{CacheDir: cache, CooprBinary: "/missing/coopr"}, artifact.Client{}, ciExchangeOptions{Scope: string(data), Attempt: 1})
	if err == nil || !strings.Contains(err.Error(), "results-repository") {
		t.Fatalf("missing repository not rejected clearly: %v", err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatal("failed preflight created staging/cache state")
	}
}

func TestIndependentJobDoesNotRequireUnrelatedResultRegistry(t *testing.T) {
	w, err := workflow.Decode(strings.NewReader("version: 1\ntasks:\n  build: {image: scratch}\n  test: {from: build}\n  lint: {image: scratch}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ci.Build(w, ci.Config{})
	if err != nil || !p.RequiresResults {
		t.Fatalf("fixture did not require cross-job results: %v", err)
	}
	job, exists := p.FindJob("lint")
	if !exists {
		t.Fatal("missing independent lint job")
	}
	scope, _ := json.Marshal(testCIScope(p.ID))
	_, err = runCIExchange(context.Background(), w, p, job, runner.Options{ContextDir: t.TempDir(), CacheDir: filepath.Join(t.TempDir(), "cache"), StateDir: filepath.Join(t.TempDir(), "state"), CooprBinary: "/missing/coopr"}, artifact.Client{}, ciExchangeOptions{Scope: string(scope), Attempt: 1})
	if err == nil || strings.Contains(err.Error(), "results-repository") || !strings.Contains(err.Error(), "coopr") {
		t.Fatalf("independent job was blocked by unrelated registry requirement: %v", err)
	}
}

func TestCompactNativeResultArrayGate(t *testing.T) {
	if err := verifyCIResults(`["success","success"]`); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`[]`, `["success","skipped"]`, `["failure"]`, `[null]`} {
		if err := verifyCIResults(raw); err == nil {
			t.Fatalf("accepted native statuses %s", raw)
		}
	}
}

func TestCIInputStagingRejectsPhysicalContextOverlap(t *testing.T) {
	root := t.TempDir()
	contextDir := filepath.Join(root, "checkout")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "cache-link")
	if err := os.Symlink(contextDir, alias); err != nil {
		t.Fatal(err)
	}
	_, err := ciInputDirectory(runner.Options{ContextDir: contextDir, CacheDir: filepath.Join(alias, "not-created")})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("cache alias entered context: %v", err)
	}
	if _, err := os.Stat(filepath.Join(contextDir, "not-created")); !os.IsNotExist(err) {
		t.Fatal("rejected cache alias created workflow inputs")
	}
}
