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

	"github.com/strio-dev/stoke/internal/ci"
)

func TestCDEventsFilePreservesCLIResultsAndLogs(t *testing.T) {
	root := t.TempDir()
	contextDir := filepath.Join(root, "source")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(contextDir, "stoke.yaml")
	data := fmt.Sprintf("version: 1\ntasks:\n  verify:\n    platform: %s/%s\n    strategy: {matrix: {mode: [one, two]}}\n    host: {tools: [/bin/sh]}\n    steps: [{run: [/bin/sh, -c, \"printf 'USER_VISIBLE_LOG\\n'\"]}]\n", runtime.GOOS, runtime.GOARCH)
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	if err := os.Mkdir(os.Getenv("TMPDIR"), 0700); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, "events.jsonl")
	args := []string{"run", "-f", file, "--allow-host", "--events", events, "verify"}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, &stderr)
	}
	var report struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Status != "passed" || !strings.Contains(stderr.String(), "USER_VISIBLE_LOG") {
		t.Fatalf("logs/results changed: %s %s (%v)", &stdout, &stderr, err)
	}
	info, err := os.Stat(events)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("events not a private file: %v %v", info, err)
	}
	stream, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(stream, []byte("\n")); lines != 6 || bytes.Contains(stream, []byte("USER_VISIBLE_LOG")) {
		t.Fatalf("expected pipeline/task lifecycle without raw logs: %s", stream)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("existing event file overwritten: code=%d stderr=%s", code, &stderr)
	}
	if current, err := os.ReadFile(events); err != nil || !bytes.Equal(current, stream) {
		t.Fatal("existing event evidence was changed", err)
	}
	args[5] = filepath.Join(contextDir, "events.jsonl")
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "context") {
		t.Fatalf("event file admitted into source inputs: code=%d stderr=%s", code, &stderr)
	}
	failed := fmt.Sprintf("version: 1\ntasks:\n  bad:\n    platform: %s/%s\n    host: {tools: [/bin/sh]}\n    steps: [{run: [/bin/sh, -c, 'exit 7']}]\n  blocked:\n    platform: %s/%s\n    needs: [bad]\n    host: {tools: [/bin/sh]}\n    steps: [{run: [/bin/sh, -c, true]}]\n", runtime.GOOS, runtime.GOARCH, runtime.GOOS, runtime.GOARCH)
	if err := os.WriteFile(file, []byte(failed), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "failed.jsonl")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run", "-f", file, "--allow-host", "--events", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("failed task became successful: %d %s", code, &stderr)
	}
	stream, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(stream), []byte("\n"))
	var terminal struct {
		Subject    struct{ Content struct{ Outcome string } }
		CustomData struct{ Skipped []string }
	}
	if len(lines) != 4 {
		t.Fatalf("unstarted task fabricated lifecycle events: %s", stream)
	}
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Subject.Content.Outcome != "failure" || len(terminal.CustomData.Skipped) != 1 || terminal.CustomData.Skipped[0] != "blocked" {
		t.Fatalf("final invocation failure/skip facts missing: %s (%v)", stream, err)
	}
}

func TestCDEventsCIUnitUsesActualRetryWithoutPipelineEvents(t *testing.T) {
	root := t.TempDir()
	contextDir := filepath.Join(root, "source")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(contextDir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	writeCIFile(t, "stoke.yaml", fmt.Sprintf("version: 1\ntasks:\n  opaque-name:\n    platform: %s/%s\n    host: {tools: [/bin/sh]}\n    steps: [{run: [/bin/sh, -c, true]}]\n", runtime.GOOS, runtime.GOARCH))
	code, output, diagnostics := invokeCI([]string{"ci", "plan"})
	var plan ci.Plan
	if err := json.Unmarshal([]byte(output), &plan); code != 0 || err != nil {
		t.Fatalf("plan: %d %s %v", code, diagnostics, err)
	}
	planPath := filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, []byte(output), 0600); err != nil {
		t.Fatal(err)
	}
	scope, err := json.Marshal(testCIScope(plan.ID))
	if err != nil {
		t.Fatal(err)
	}
	var priorSubject string
	for _, attempt := range []int{2, 3} {
		path := filepath.Join(root, fmt.Sprintf("events-%d.jsonl", attempt))
		args := []string{"ci", "run", "--plan", planPath, "--job", plan.Groups[0].Jobs[0].ID, "--scope", string(scope), "--attempt", fmt.Sprint(attempt), "--allow-host", "--events", path}
		if code, _, stderr := invokeCI(args); code != 0 {
			t.Fatalf("CI run: %d %s", code, stderr)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
		if len(lines) != 2 || bytes.Contains(data, []byte("pipelinerun")) {
			t.Fatalf("isolated unit fabricated pipeline facts: %s", data)
		}
		var event struct {
			Subject    struct{ Id string }
			CustomData struct {
				CI struct {
					Attempt int
					Unit    string
				}
			}
		}
		if err := json.Unmarshal(lines[0], &event); err != nil || event.CustomData.CI.Attempt != attempt || event.CustomData.CI.Unit != plan.Groups[0].Jobs[0].ID || event.Subject.Id == priorSubject {
			t.Fatalf("real retry identity missing: %s (%v)", data, err)
		}
		priorSubject = event.Subject.Id
	}
}

func TestCDEventsCannotWriteIntoNamedOrLiveInputs(t *testing.T) {
	for _, declaration := range []string{"contexts: {assets: %q}", "mounts: [{type: host, source: %q, target: /input}]", "host: {tools: [/bin/sh], toolchain: [%q]}"} {
		t.Run(declaration[:strings.Index(declaration, ":")], func(t *testing.T) {
			root := t.TempDir()
			contextDir, input := filepath.Join(root, "source"), filepath.Join(root, "external-input")
			for _, dir := range []string{contextDir, input} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			workflow := filepath.Join(contextDir, "stoke.yaml")
			data := "version: 1\ntasks:\n  work:\n    image: scratch\n    " + fmt.Sprintf(declaration, input) + "\n    steps: [{run: [not-executed]}]\n"
			if strings.HasPrefix(declaration, "host:") {
				data = fmt.Sprintf("version: 1\ntasks:\n  work:\n    platform: %s/%s\n    %s\n    steps: [{run: [/bin/sh, -c, true]}]\n", runtime.GOOS, runtime.GOARCH, fmt.Sprintf(declaration, input))
			}
			if err := os.WriteFile(workflow, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			events := filepath.Join(input, "events.jsonl")
			var stdout, stderr bytes.Buffer
			if code := run([]string{"run", "-f", workflow, "--allow-host", "--coopr-binary", "/missing/coopr", "--events", events}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "overlaps") {
				t.Fatalf("source overlap admitted: code=%d %s", code, &stderr)
			}
			if _, err := os.Stat(events); !os.IsNotExist(err) {
				t.Fatalf("observer changed an execution input before admission: %v", err)
			}
		})
	}
}
