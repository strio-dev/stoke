package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
	"github.com/strio-dev/stoke/internal/runner"
)

func TestHumanDiagnosticsPreserveRunJSONAndGatedRetry(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
			t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
			contextDir := filepath.Join(root, "context")
			if err := os.Mkdir(contextDir, 0700); err != nil {
				t.Fatal(err)
			}
			command := "printf 'fragment'; printf 'ed\\nsecond\\nlast'; printf errtail >&2"
			if fail {
				command += "; exit 7"
			}
			document := fmt.Sprintf("version: 1\ntasks:\n  check:\n    platform: %s/%s\n    host: {tools: [/bin/sh]}\n    steps: [{run: [/bin/sh, -c, %q]}]\n  artifact:\n    platform: %s/%s\n    needs: [check]\n    host: {tools: [/bin/sh]}\n    steps: [{run: [/bin/sh, -c, 'echo packaged']}]\n", runtime.GOOS, runtime.GOARCH, command, runtime.GOOS, runtime.GOARCH)
			file := filepath.Join(contextDir, "stoke.yaml")
			if err := os.WriteFile(file, []byte(document), 0600); err != nil {
				t.Fatal(err)
			}
			summary := filepath.Join(root, "summary.md")
			if err := os.WriteFile(summary, nil, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GITHUB_ACTIONS", "true")
			t.Setenv("GITHUB_STEP_SUMMARY", summary)
			output, events := filepath.Join(root, "outputs"), filepath.Join(root, "events.jsonl")
			var out, diagnostics bytes.Buffer
			code := run([]string{"run", "-f", file, "--allow-host", "--output", output, "--events", events, "artifact"}, &out, &diagnostics)
			wantCode, wantStatus := 0, "passed"
			if fail {
				wantCode, wantStatus = 1, "failed"
			}
			var report runner.Report
			if code != wantCode || json.Unmarshal(out.Bytes(), &report) != nil || report.Status != wantStatus {
				t.Fatalf("exit/JSON contract: %d %s %s", code, &out, &diagnostics)
			}
			for _, line := range []string{"[check] fragmented\n", "[check] second\n", "[check] last\n", "[check] errtail\n"} {
				if !strings.Contains(diagnostics.String(), line) {
					t.Fatalf("human log lost attribution: %s", &diagnostics)
				}
			}
			captured, err := os.ReadFile(filepath.Join(output, "check", "stdout.log"))
			if err != nil || string(captured) != "fragmented\nsecond\nlast" {
				t.Fatalf("raw captured stdout changed: %q %v", captured, err)
			}
			stepSummary, err := os.ReadFile(summary)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				for _, text := range []string{"Failed tasks: check", filepath.Join(report.Directory, "run.json"), "original gated targets: artifact", "fresh --output", "fresh --events"} {
					if !strings.Contains(diagnostics.String(), text) || !strings.Contains(string(stepSummary), text) {
						t.Fatalf("failure guidance missing %q: %s / %s", text, &diagnostics, stepSummary)
					}
				}
				if report.Tasks[1].Status != "skipped" || strings.Contains(diagnostics.String(), "[artifact] packaged") {
					t.Fatal("failed prerequisite did not block artifact")
				}
			} else if len(stepSummary) != 0 || strings.Contains(diagnostics.String(), "Retry") {
				t.Fatal("successful invocation received failure guidance")
			}
		})
	}
}

func TestProviderRetryGuidanceAndSelectedDestinationSafety(t *testing.T) {
	file := filepath.Join(t.TempDir(), "summary")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_STEP_SUMMARY", file)
	report := &runner.Report{Directory: "/retained", Tasks: []runner.TaskResult{{Name: "test--matrix-stable-id", Task: "test", Matrix: map[string]string{"mode": "one"}, Status: "failed", Exports: []cooprwire.ExportResult{{Destination: "reports/<test>", Status: "passed"}}}}}
	before, _ := json.Marshal(report)
	var out bytes.Buffer
	printFailureSummary(&out, report, nil, runner.Options{OutputDir: "/prior-output", EventPath: "/prior-events"}, true)
	for _, text := range []string{"Failed tasks: test[mode=one]", "GitHub's Re-run job", "native prerequisites", "rerun the workflow and its gates", "must be absent", "reports/<test>"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("missing safe native retry guidance %q: %s", text, &out)
		}
	}
	afterJSON, _ := json.Marshal(report)
	if !bytes.Equal(before, afterJSON) {
		t.Fatal("human summary mutated machine-readable task identity")
	}
	if strings.Contains(out.String(), "ci run --job") || strings.Contains(out.String(), "fresh --output") {
		t.Fatal("native retry suggested editing invocation/gates")
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), "<test>") || !strings.Contains(string(data), "&lt;test&gt;") {
		t.Fatal("untrusted Markdown/HTML was not escaped")
	}
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GITLAB_CI", "true")
	out.Reset()
	printFailureSummary(&out, report, []string{"artifact"}, runner.Options{}, false)
	after, _ := os.ReadFile(file)
	if !bytes.Equal(data, after) || !strings.Contains(out.String(), "original gated targets: artifact") || strings.Contains(out.String(), "GitHub") {
		t.Fatal("GitLab direct invocation acquired GitHub retry behavior")
	}
	report.Tasks[0].Status = "passed"
	out.Reset()
	printFailureSummary(&out, report, []string{"artifact"}, runner.Options{}, false)
	if strings.Contains(out.String(), "Failed tasks:") || !strings.Contains(out.String(), "Workflow failed") {
		t.Fatal("post-task cleanup/report failure fabricated a failed task")
	}
}

func TestNativeCIFailureSummaryKeepsReportJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(state, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(state, "cache"))
	writeCIFile(t, "stoke.yaml", fmt.Sprintf("version: 1\ntasks:\n  check:\n    platform: %s/%s\n    host: {tools: [/bin/sh]}\n    steps: [{run: [/bin/sh, -c, 'echo failed; exit 7']}]\n", runtime.GOOS, runtime.GOARCH))
	summary := filepath.Join(t.TempDir(), "summary")
	if err := os.WriteFile(summary, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	code, out, stderr := invokeCI([]string{"run", "--ci", "--allow-host", "check"})
	var report runner.Report
	if code != 1 || json.Unmarshal([]byte(out), &report) != nil || report.Status != "failed" {
		t.Fatalf("native CI JSON/exit contract: %d %s %s", code, out, stderr)
	}
	data, _ := os.ReadFile(summary)
	if !strings.Contains(stderr, "Failed tasks: check") || !strings.Contains(string(data), "Re-run job") || !strings.Contains(string(data), filepath.Join(report.Directory, "run.json")) {
		t.Fatalf("native failure summary missing: %s / %s", stderr, data)
	}
}
