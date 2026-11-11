package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/stoke/internal/workflow"
)

// TestReusableInputsWithCoopr verifies authored OCI definitions at invocation
// time, including prerequisite isolation and Coopr-owned cache invalidation.
func TestReusableInputsWithCoopr(t *testing.T) {
	if testing.Short() {
		t.Skip("requires configured rootless host and packaged Stoke/Coopr executables")
	}
	binary, cooprBinary, busybox := os.Getenv("STOKE_BINARY"), os.Getenv("STOKE_COOPR_BINARY"), os.Getenv("STOKE_SMOKE_BUSYBOX")
	for name, value := range map[string]string{"STOKE_BINARY": binary, "STOKE_COOPR_BINARY": cooprBinary, "STOKE_SMOKE_BUSYBOX": busybox} {
		if value == "" {
			t.Fatalf("%s must select the configured acceptance executable", name)
		}
		if _, err := os.Stat(value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	work, err := os.MkdirTemp("", "stoke-inputs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Retained reusable-input acceptance evidence: %s", work)
	checks, reports := []string{}, map[string]string{}
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	encode := func(value any) []byte {
		t.Helper()
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(data, '\n')
	}
	defer func() {
		write(filepath.Join(work, "summary.json"), encode(map[string]any{"passed": !t.Failed(), "evidence": work, "checks": checks, "reports": reports}), 0600)
	}()
	registry := registryFixture(t)
	host := strings.TrimPrefix(registry.URL, "http://")
	command := func(label string, args []string, env []string) ([]byte, []byte) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		write(filepath.Join(work, label+".stdout"), stdout.Bytes(), 0600)
		write(filepath.Join(work, label+".stderr"), stderr.Bytes(), 0600)
		if err != nil {
			t.Fatalf("%s: %v\n%s", label, err, &stderr)
		}
		return stdout.Bytes(), stderr.Bytes()
	}
	publisher := filepath.Join(work, "publisher")
	native := `arg "VERSION"
arg "MODE"
from "scratch" {
    arg "VERSION"
    arg "MODE"
    copy "busybox" "/bin/busybox"
    env NATIVE_VERSION="${VERSION}" NATIVE_MODE="${MODE}"
    run {
        exec "/bin/busybox" "sh" "-eu" "-c" "printf '%s\\n%s\\n' \"$NATIVE_VERSION\" \"$NATIVE_MODE\" > /native.txt; echo STOKE_INPUT_NATIVE_EXECUTED_$NATIVE_VERSION"
    }
}
`
	write(filepath.Join(publisher, "native.coopr"), []byte(native), 0644)
	library := `version: 1
inputs:
  version: {type: string, required: true}
  mode: {type: string, default: release}
assets: [native.coopr]
tasks:
  native:
    coopr:
      files: [native.coopr]
      files-from: component
      args:
        VERSION: {input: version}
        MODE: {input: mode}
  cached:
    image: scratch
    env:
      VERSION: {input: version}
      MODE: {input: mode}
    steps:
      - copy: {sources: [busybox], destination: /bin/busybox}
      - run: [/bin/busybox, sh, -eu, -c, 'printf "%s\n%s\n" "$VERSION" "$MODE" > /literal.txt; echo STOKE_INPUT_LITERAL_EXECUTED_$VERSION']
  verify:
    from: native
    cache: false
    strategy:
      max-parallel: 2
      matrix: {label: [one, two]}
    env:
      EXPECTED_VERSION: {input: version}
      EXPECTED_MODE: {input: mode}
    steps:
      - copy: {from: cached, sources: [/literal.txt], destination: /literal.txt}
      - run: [/bin/busybox, sh, -eu, -c, 'test "$(/bin/busybox cat /native.txt)" = "$(printf "%s\n%s\n" "$EXPECTED_VERSION" "$EXPECTED_MODE")"; /bin/busybox cmp /native.txt /literal.txt; printf "%s\n%s\n%s\n" "$EXPECTED_VERSION" "$EXPECTED_MODE" "$STOKE_MATRIX_LABEL" > /selected.txt']
`
	libraryYAML := filepath.Join(publisher, "stoke.yaml")
	write(libraryYAML, []byte(library), 0600)
	stdout, _ := command("publish-library", []string{"publish", "--plain-http", "-f", libraryYAML, host + "/library:inputs"}, os.Environ())
	libraryReference := strings.TrimSpace(string(stdout))
	pulled := filepath.Join(work, "pulled")
	command("pull-library", []string{"pull", "--plain-http", "--directory", pulled, libraryReference}, os.Environ())
	decoded, err := workflow.Decode(bytes.NewReader(read(filepath.Join(pulled, "stoke.yaml"))))
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Inputs["version"].Required || decoded.Inputs["mode"].Default == nil || *decoded.Inputs["mode"].Default != "release" || decoded.Tasks["native"].Coopr.Args["VERSION"].Input != "version" || decoded.Tasks["cached"].Env["VERSION"].Input != "version" {
		t.Fatal("OCI publication/pull resolved or discarded reusable input declarations")
	}
	if string(read(filepath.Join(pulled, "native.coopr"))) != native {
		t.Fatal("OCI input component lost its native definition")
	}
	checks = append(checks, "oci-publish-pull-retains-required-default-and-bindings")
	if err := os.RemoveAll(publisher); err != nil {
		t.Fatal(err)
	}
	caller := filepath.Join(work, "caller")
	write(filepath.Join(caller, "busybox"), read(busybox), 0755)
	policy := filepath.Join(work, "policy.json")
	write(policy, []byte(`{"default":[{"type":"reject"}],"transports":{"oci":{"":[{"type":"insecureAcceptAnything"}]},"oci-archive":{"":[{"type":"insecureAcceptAnything"}]},"containers-storage":{"":[{"type":"insecureAcceptAnything"}]}}}`), 0600)
	rootYAML := filepath.Join(work, "consumer.yaml")
	writeRoot := func(first string) {
		write(rootYAML, encode(map[string]any{"version": 1, "imports": map[string]string{"library": libraryReference}, "tasks": map[string]any{
			"first":  map[string]any{"uses": "library/verify", "with": map[string]string{"version": first}},
			"second": map[string]any{"uses": "library/verify", "with": map[string]string{"version": "second", "mode": "custom"}},
		}}), 0600)
	}
	env := append(os.Environ(), "STOKE_COOPR_BINARY="+cooprBinary,
		"XDG_STATE_HOME="+filepath.Join(work, "state"), "XDG_CACHE_HOME="+filepath.Join(work, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(work, "config"), "XDG_DATA_HOME="+filepath.Join(work, "data"))
	run := func(label, first string) (ociRuntimeReport, []byte) {
		t.Helper()
		stdout, stderr := command(label, []string{"run", "--plain-http", "--context", caller, "--signature-policy", policy, "-f", rootYAML}, env)
		var report ociRuntimeReport
		if err := json.Unmarshal(stdout, &report); err != nil {
			t.Fatal(err)
		}
		if report.Status != "passed" || len(report.Tasks) != 8 {
			t.Fatalf("%s status=%s tasks=%d, expected eight isolated prerequisite/matrix jobs", label, report.Status, len(report.Tasks))
		}
		reports[label] = report.Directory
		write(filepath.Join(work, label+".run.json"), stdout, 0600)
		selected := 0
		for _, task := range report.Tasks {
			if task.Status != "passed" {
				t.Fatalf("%s: failed task %+v", label, task)
			}
			if task.Task != "first" && task.Task != "second" {
				continue
			}
			version, mode := first, "release"
			if task.Task == "second" {
				version, mode = "second", "custom"
			}
			want := fmt.Sprintf("%s\n%s\n%s\n", version, mode, task.Matrix["label"])
			if got := string(read(filepath.Join(task.Filesystem, "selected.txt"))); got != want {
				t.Fatalf("%s/%s inputs leaked: got %q want %q", label, task.Name, got, want)
			}
			selected++
		}
		if selected != 4 {
			t.Fatalf("%s: got %d selected matrix jobs, expected four", label, selected)
		}
		return report, stderr
	}
	cached := func(report ociRuntimeReport, invocation string) *ociRuntimeTask {
		t.Helper()
		for n := range report.Tasks {
			task := &report.Tasks[n]
			if strings.HasPrefix(task.Task, invocation+"--") && strings.HasSuffix(task.Task, "--cached") {
				if task.Cache == nil {
					t.Fatalf("%s literal prerequisite omitted available cache counters", invocation)
				}
				return task
			}
		}
		t.Fatalf("%s literal prerequisite missing", invocation)
		return nil
	}
	executed := func(log []byte, kind, version string) bool {
		marker := []byte("STOKE_INPUT_" + kind + "_EXECUTED_" + version)
		for line := range bytes.SplitSeq(log, []byte("\n")) {
			if bytes.Equal(line, marker) {
				return true
			}
		}
		return false
	}
	writeRoot("first")
	cold, coldLog := run("cold", "first")
	for _, version := range []string{"first", "second"} {
		for _, kind := range []string{"NATIVE", "LITERAL"} {
			if !executed(coldLog, kind, version) {
				t.Fatalf("cold %s/%s RUN did not execute", kind, version)
			}
		}
	}
	checks = append(checks, "two-invocations-isolate-native-literal-prerequisites-and-matrix-values")
	warm, warmLog := run("warm", "first")
	if cold.Directory == warm.Directory {
		t.Fatal("warm invocation reused the native run directory")
	}
	for _, version := range []string{"first", "second"} {
		for _, kind := range []string{"NATIVE", "LITERAL"} {
			if executed(warmLog, kind, version) {
				t.Fatalf("unchanged %s/%s RUN executed instead of reusing cache", kind, version)
			}
		}
	}
	for _, invocation := range []string{"first", "second"} {
		task := cached(warm, invocation)
		if task.Cache.Hits == 0 || task.Cache.Misses != 0 {
			t.Fatalf("warm %s cache=%+v", invocation, task.Cache)
		}
	}
	checks = append(checks, "same-effective-inputs-reuse-coopr-native-and-literal-cache")
	writeRoot("changed")
	changed, changedLog := run("changed-first", "changed")
	for _, kind := range []string{"NATIVE", "LITERAL"} {
		if !executed(changedLog, kind, "changed") {
			t.Fatalf("changed first %s RUN did not invalidate", kind)
		}
		if executed(changedLog, kind, "second") {
			t.Fatalf("unchanged second %s RUN was invalidated by its sibling input", kind)
		}
	}
	if task := cached(changed, "first"); task.Cache.Misses == 0 {
		t.Fatal("changed input did not produce a literal cache miss")
	}
	if task := cached(changed, "second"); task.Cache.Hits == 0 || task.Cache.Misses != 0 {
		t.Fatalf("unchanged sibling cache=%+v", task.Cache)
	}
	checks = append(checks, "changed-input-invalidates-only-its-invocation")
}
