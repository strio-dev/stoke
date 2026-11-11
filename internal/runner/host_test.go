package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/hostfiles"
	"github.com/strio-dev/stoke/internal/workflow"
)

func hostFixture(t *testing.T, configuration string) *workflow.Workflow {
	t.Helper()
	return fixture(t, strings.ReplaceAll(configuration, "NATIVE", runtime.GOOS+"/"+runtime.GOARCH))
}

func hostRunOptions(t *testing.T) Options {
	t.Helper()
	options := testOptions(t)
	options.AllowHost = true
	options.CooprBinary = "/must-not-start-coopr-for-host-tasks"
	return options
}

func TestHostAuthorityFreshGatesAndCapturedData(t *testing.T) {
	w := hostFixture(t, `version: 1
tasks:
  producer:
    platform: NATIVE
    host:
      tools: [/bin/sh]
      outputs: [out]
    steps:
      - run: [/bin/sh, -c, 'mkdir out; printf exact > out/value']
  consumer:
    platform: NATIVE
    host:
      tools: [/bin/sh]
      inputs: [{from: producer, path: out, destination: input}]
      outputs: [reports]
    steps:
      - run: [/bin/sh, -c, 'test "$(cat input/value)" = exact; printf fresh']
        stdout: reports/checked
`)
	options := hostRunOptions(t)
	denied := options
	denied.AllowHost = false
	if _, err := Run(context.Background(), w, nil, denied); err == nil || !strings.Contains(err.Error(), "--allow-host") {
		t.Fatalf("authority: %v", err)
	}
	if _, err := os.Stat(options.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied execution created state", err)
	}
	for range 2 {
		report, err := Run(context.Background(), w, nil, options)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Tasks) != 2 || report.Tasks[0].Kind != "host" || report.Tasks[1].Status != "passed" {
			t.Fatalf("%+v", report)
		}
		data, err := os.ReadFile(filepath.Join(report.Tasks[1].Filesystem, "reports/checked"))
		if err != nil || string(data) != "fresh" {
			t.Fatal(string(data), err)
		}
		if string(report.Tasks[1].Metadata["stoke.host.execution"]) != `"executed"` {
			t.Fatal("host was not freshly executed")
		}
		if _, err := hostfiles.Validate(context.Background(), report.Tasks[1].OCI, report.Tasks[1].Digest); err != nil {
			t.Fatal(err)
		}
	}
	w.Tasks["producer"] = workflow.Task{Host: &workflow.Host{Tools: []string{"/bin/sh"}}, Platform: w.Tasks["producer"].Platform, Steps: []workflow.Step{{Run: []string{"/bin/sh", "-c", "exit 7"}}}}
	// Remove the data edge; the consumer remains a pure success gate.
	consumer := w.Tasks["consumer"]
	consumer.Host.Inputs = nil
	consumer.Needs = []string{"producer"}
	w.Tasks["consumer"] = consumer
	report, err := Run(context.Background(), w, nil, options)
	if err == nil || report.Tasks[1].Status != "skipped" {
		t.Fatalf("gate: %+v %v", report, err)
	}
}

func TestHostCacheRestoresAndInvalidatesDeclaredInputs(t *testing.T) {
	w := hostFixture(t, `version: 1
tasks:
  build:
    platform: NATIVE
    cache: true
    env: {MODE: first}
    host:
      tools: [/bin/sh]
      inputs: [{path: source}]
      outputs: [out]
      effects: captured
      toolchain: [/bin/sh]
    steps:
      - run: [/bin/sh, -c, 'mkdir out; printf "executed\n"; cat source > out/value; printf "%s" "$MODE" >> out/value']
`)
	options := hostRunOptions(t)
	var output bytes.Buffer
	options.Stdout = &output
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(options.ContextDir, "source"), []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("original")
	first, err := Run(context.Background(), w, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), w, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if second.Tasks[0].Cache == nil || second.Tasks[0].Cache.Hits != 1 || second.Tasks[0].Digest != first.Tasks[0].Digest {
		t.Fatalf("no cache restoration: %+v", second)
	}
	data, err := os.ReadFile(filepath.Join(second.Tasks[0].Filesystem, "out/value"))
	if err != nil || string(data) != "originalfirst" {
		t.Fatal(string(data), err)
	}
	if strings.Count(output.String(), "executed") != 1 {
		t.Fatal(output.String())
	}
	write("changed")
	third, err := Run(context.Background(), w, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if third.Tasks[0].Cache.Misses != 1 || third.Tasks[0].Digest == first.Tasks[0].Digest {
		t.Fatal("content did not invalidate")
	}
	task := w.Tasks["build"]
	value := "second"
	task.Env["MODE"] = workflow.Value{Value: &value}
	w.Tasks["build"] = task
	fourth, err := Run(context.Background(), w, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Tasks[0].Cache.Misses != 1 {
		t.Fatal("environment did not invalidate")
	}
	options.NoCache = true
	fifth, err := Run(context.Background(), w, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if string(fifth.Tasks[0].Metadata["stoke.host.execution"]) != `"executed"` {
		t.Fatal("--no-cache reused result")
	}
}

func TestHostRequirementsAndStdoutConfinement(t *testing.T) {
	w := hostFixture(t, `version: 1
tasks:
  check:
    platform: NATIVE
    host: {tools: [/bin/sh], capabilities: [provisioned-toolchain]}
    steps: [{run: [/bin/sh, -c, 'printf okay']}]
`)
	options := hostRunOptions(t)
	if _, err := Run(context.Background(), w, nil, options); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatal(err)
	}
	options.HostCapabilities = []string{"provisioned-toolchain"}
	if _, err := Run(context.Background(), w, nil, options); err != nil {
		t.Fatal(err)
	}
	task := w.Tasks["check"]
	task.Host.Capabilities = nil
	outside := t.TempDir()
	v := outside
	task.Env = map[string]workflow.Value{"OUTSIDE": {Value: &v}}
	task.Steps = []workflow.Step{{Run: []string{"/bin/sh", "-c", `ln -s "$OUTSIDE" reports`}}, {Run: []string{"/bin/sh", "-c", "printf bad"}, Stdout: "reports/output"}}
	w.Tasks["check"] = task
	if _, err := Run(context.Background(), w, nil, options); err == nil {
		t.Fatal("accepted escaping stdout path")
	}
	if _, err := os.Stat(filepath.Join(outside, "output")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stdout escaped", err)
	}
}

func TestHostPreflightRejectsUnavailableToolsPlatformsAndStorageOverlap(t *testing.T) {
	native := runtime.GOOS + "/" + runtime.GOARCH
	task := workflow.Task{Platform: &workflow.Platform{Value: &native}, Host: &workflow.Host{Tools: []string{"/bin/sh"}}}
	options := hostRunOptions(t)
	task.Host.Tools = []string{filepath.Join(t.TempDir(), "unprovisioned")}
	if err := validateHostExecution(task, options); err == nil {
		t.Fatal("unavailable host tool accepted")
	}
	task.Host.Tools = []string{"/bin/sh"}
	foreign := "linux/arm64"
	if runtime.GOARCH == "arm64" {
		foreign = "linux/amd64"
	}
	task.Platform = &workflow.Platform{Value: &foreign}
	if err := validateHostExecution(task, options); err == nil || !strings.Contains(err.Error(), "native") {
		t.Fatal("foreign host platform accepted", err)
	}
	task.Platform = &workflow.Platform{Value: &native}
	yes := true
	task.Cache = &yes
	t.Setenv("TMPDIR", t.TempDir())
	parent, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	options.CacheDir = filepath.Join(parent, fmt.Sprintf("stoke-host-%d", os.Getuid()), "cache")
	if err := validateHostExecution(task, options); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatal("host cache inside execution namespace accepted", err)
	}
}

func TestHostPreservesDeclaredExecutableArgvZero(t *testing.T) {
	alias := filepath.Join(t.TempDir(), "sh")
	if err := os.Symlink("/bin/sh", alias); err != nil {
		t.Fatal(err)
	}
	w := hostFixture(t, fmt.Sprintf(`version: 1
tasks:
  check:
    platform: NATIVE
    host: {tools: [%q], outputs: [argv0]}
    steps:
      - run: [%q, -c, 'printf %%s "$0"']
        stdout: argv0
`, alias, alias))
	report, err := Run(t.Context(), w, nil, hostRunOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(report.Tasks[0].Filesystem, "argv0"))
	if err != nil || string(actual) != alias {
		t.Fatal("resolved executable changed authored argv[0]", string(actual), alias, err)
	}
}

func TestHostLogBudgetCancelsCommandWithoutUnboundedWrites(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	writer := hostLogWriter{remaining: 3, writer: &output, cancel: cancel}
	if n, err := writer.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := writer.Write([]byte("overflow")); n != 0 || err == nil || ctx.Err() == nil {
		t.Fatal("log limit failed to cancel", n, err, ctx.Err())
	}
	if output.String() != "abc" {
		t.Fatal("over-budget bytes escaped", output.String())
	}
}

func TestHostWorkspaceLeaseCancelsAndPreservesStaleData(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	root, lock, err := leaseHostWorkspace(t.Context(), "declared-input", "producer")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, other, err := leaseHostWorkspace(ctx, "declared-input", "producer"); !errors.Is(err, context.DeadlineExceeded) || other != nil {
		t.Fatal("competing workspace lease did not cancel", other, err)
	}
	if err := os.WriteFile(filepath.Join(root, "preserve"), []byte("stale owned evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, other, err := leaseHostWorkspace(t.Context(), "declared-input", "producer"); err == nil || other != nil || !strings.Contains(err.Error(), "retained data") {
		t.Fatal("stale execution workspace was reused", other, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "preserve"))
	if err != nil || string(data) != "stale owned evidence" {
		t.Fatal("stale data was altered", string(data), err)
	}
}

func TestHostCancellationAndNormalExitDrainChildren(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process supervision")
	}
	for _, cancelled := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			options := hostRunOptions(t)
			marker := filepath.Join(t.TempDir(), "pid")
			command := `sleep 600 </dev/null >/dev/null 2>&1 & echo $! > "$PIDFILE"`
			if cancelled {
				command = `trap '' TERM; sleep 600 </dev/null >/dev/null 2>&1 & echo $! > "$PIDFILE"; wait`
			}
			yaml := fmt.Sprintf("version: 1\ntasks:\n  check:\n    platform: NATIVE\n    env: {PIDFILE: %q}\n    host: {tools: [/bin/sh]}\n    steps:\n      - run: [/bin/sh, -c, %q]\n", marker, command)
			w := hostFixture(t, yaml)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := Run(ctx, w, nil, options); done <- err }()
			deadline := time.Now().Add(5 * time.Second)
			var raw []byte
			for time.Now().Before(deadline) {
				var err error
				raw, err = os.ReadFile(marker)
				if err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(raw) == 0 {
				cancel()
				t.Fatal("child readiness missing")
			}
			if cancelled {
				cancel()
			}
			select {
			case err := <-done:
				if cancelled && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if !cancelled && err != nil {
					t.Fatal(err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("host did not drain")
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			status, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if err == nil {
				end := strings.LastIndex(string(status), ") ")
				fields := strings.Fields(string(status[end+2:]))
				if fields[0] != "Z" && fields[0] != "X" {
					t.Fatalf("child %d still alive: %s", pid, status)
				}
			}
		})
	}
}

func TestHostCIImportsExactFilesWithoutEngine(t *testing.T) {
	options := hostRunOptions(t)
	w := hostFixture(t, `version: 1
tasks:
  producer:
    platform: NATIVE
    host: {tools: [/bin/sh], outputs: [out]}
    steps: [{run: [/bin/sh, -c, 'mkdir out; printf exact > out/data']}]
  test:
    platform: NATIVE
    host:
      tools: [/bin/sh]
      inputs: [{from: producer, path: out}]
    steps: [{run: [/bin/sh, -c, 'test "$(cat out/data)" = exact']}]
`)
	produced, err := Run(context.Background(), w, []string{"producer"}, options)
	if err != nil {
		t.Fatal(err)
	}
	result := produced.Tasks[0]
	request := CIRequest{Execute: []string{"test"}, Imports: map[string]CIImport{"producer": {Kind: "host", OCI: result.OCI, Digest: result.Digest, Platform: result.Platform, CanonicalReference: "registry.example/results@" + result.Digest}}}
	imported, err := RunCI(context.Background(), w, request, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Tasks) != 1 || imported.Tasks[0].Name != "test" || imported.Status != "passed" {
		data, _ := json.Marshal(imported)
		t.Fatal(string(data))
	}
	delete(request.Imports, "producer")
	if _, err := RunCI(context.Background(), w, request, options); err == nil || !strings.Contains(err.Error(), "exact data prerequisite") {
		t.Fatal("missing result replayed", err)
	}
}

func TestHostCacheBindsEffectiveLocationsAndCleansReadonlyInputs(t *testing.T) {
	options := hostRunOptions(t)
	if err := os.Mkdir(filepath.Join(options.ContextDir, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(options.ContextDir, "source/value"), []byte("source"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(options.ContextDir, "source"), 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(options.ContextDir, "source"), 0700) })
	w := hostFixture(t, `version: 1
tasks:
  check:
    platform: NATIVE
    cache: true
    host:
      tools: [/bin/sh]
      inputs: [{path: source}]
      outputs: [out]
      effects: captured
      toolchain: [/bin/sh]
    steps:
      - run: [/bin/sh, -c, 'mkdir -p out/readonly; printf "%s\n%s\n%s" "$HOME" "$TMPDIR" "$PWD" > out/readonly/locations; chmod 0555 out/readonly']
`)
	first, err := Run(context.Background(), w, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), w, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if second.Tasks[0].Cache.Hits != 1 {
		t.Fatal("stable location did not permit reuse")
	}
	options.NoCache = true
	forced, err := Run(context.Background(), w, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	read := func(report *Report) []byte {
		t.Helper()
		retained := filepath.Join(report.Tasks[0].Filesystem, "out/readonly")
		t.Cleanup(func() { _ = os.Chmod(retained, 0700) })
		data, err := os.ReadFile(filepath.Join(report.Tasks[0].Filesystem, "out/readonly/locations"))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	_ = read(second)
	if !bytes.Equal(read(first), read(forced)) {
		t.Fatal("effective runtime locations differed under the same reusable key")
	}
	locations := strings.Split(string(read(first)), "\n")
	if len(locations) != 3 {
		t.Fatal(locations)
	}
	for _, location := range locations {
		if _, err := os.Stat(location); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private execution workspace leaked", location, err)
		}
	}
	bad := *w
	bad.Tasks = map[string]workflow.Task{"check": w.Tasks["check"]}
	task := bad.Tasks["check"]
	task.Host = task.Host.Clone()
	task.Host.Inputs = []workflow.HostInput{{Path: "."}}
	bad.Tasks["check"] = task
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatal("ambiguous cache root accepted", err)
	}
}

func TestHostCIImportRejectsUndeclaredCapturedRoots(t *testing.T) {
	w := hostFixture(t, `version: 1
tasks:
  producer:
    platform: NATIVE
    host: {tools: [/bin/sh], outputs: [out]}
    steps: [{run: [/bin/sh, -c, true]}]
`)
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "out"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "extra"), []byte("undeclared"), 0600); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(t.TempDir(), "oci")
	identity, err := hostfiles.Capture(t.Context(), workspace, []string{"out", "extra"}, layout, runtime.GOOS+"/"+runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	err = validateCIImport(t.Context(), CIImport{Kind: "host", OCI: layout, Digest: identity, Platform: runtime.GOOS + "/" + runtime.GOARCH, CanonicalReference: "registry.example/results@" + identity}, w.Tasks["producer"])
	if err == nil || !strings.Contains(err.Error(), "captured paths") {
		t.Fatal("undeclared output accepted", err)
	}
}

func TestHostContextInputsAreFrozenBeforeCommands(t *testing.T) {
	options := hostRunOptions(t)
	path := filepath.Join(options.ContextDir, "source")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	w := hostFixture(t, fmt.Sprintf(`version: 1
tasks:
  mutate:
    platform: NATIVE
    env: {ORIGINAL: %q}
    host: {tools: [/bin/sh], inputs: [{path: source}]}
    steps: [{run: [/bin/sh, -c, 'printf changed > "$ORIGINAL"']}]
  check:
    needs: [mutate]
    platform: NATIVE
    host: {tools: [/bin/sh], inputs: [{path: source}]}
    steps: [{run: [/bin/sh, -c, 'test "$(cat source)" = original']}]
`, path))
	if _, err := Run(t.Context(), w, nil, options); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "changed" {
		t.Fatal("uncaptured effect did not execute", string(data), err)
	}
}

func TestHostCleanupUnconfirmedRetainsExecutionInputs(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, uncertain := range []bool{false, true} {
			for _, logFailure := range []bool{false, true} {
				t.Run(fmt.Sprintf("cached=%t/uncertain=%t/logFailure=%t", cached, uncertain, logFailure), func(t *testing.T) {
					t.Setenv("TMPDIR", t.TempDir())
					w := hostFixture(t, fmt.Sprintf(`version: 1
tasks:
   build:
     platform: NATIVE
     cache: %t
     host:
       tools: [/bin/sh]
       inputs: [{path: source}]
       outputs: [out]
       effects: captured
       toolchain: [/bin/sh]
     steps:
       - run: [/bin/sh, -c, 'never executed']
`, cached))
					options := hostRunOptions(t)
					options.NoCache = true
					if err := os.WriteFile(filepath.Join(options.ContextDir, "source"), []byte("input"), 0600); err != nil {
						t.Fatal(err)
					}
					writer := &hostLogWriter{cancel: func() {}}
					if logFailure {
						options.Stdout = writer
					}
					taskDir := t.TempDir()
					processErr := errors.New("process failed")
					var roots []string
					var sourcePath string
					result, err := executeHostProcess(t.Context(), workflow.Job{Name: "build", Definition: w.Tasks["build"]}, options, taskDir, nil, func(_ context.Context, command *exec.Cmd) error {
						sourcePath = filepath.Join(command.Dir, "source")
						roots, _ = filepath.Glob(filepath.Join(taskDir, ".host-work-*"))
						roots = append(roots, command.Dir)
						for _, entry := range command.Env {
							if path, ok := strings.CutPrefix(entry, "TMPDIR="); ok {
								roots = append(roots, path)
							}
						}
						if err := os.Mkdir(filepath.Join(command.Dir, "out"), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(command.Dir, "out", "value"), []byte("unpromoted"), 0600); err != nil {
							t.Fatal(err)
						}
						_, _ = command.Stdout.Write([]byte("log"))
						if uncertain {
							return errors.Join(processErr, coopr.ErrCleanupFailed)
						}
						return processErr
					})
					if !errors.Is(err, processErr) || errors.Is(err, coopr.ErrCleanupFailed) != uncertain || logFailure && !errors.Is(err, writer.err) {
						t.Fatalf("lost process/log/cleanup error: %v", err)
					}
					if len(roots) < 3 {
						t.Fatal("execution roots were not observed", roots)
					}
					for _, root := range roots {
						_, statErr := os.Stat(root)
						if uncertain && statErr != nil || !uncertain && !errors.Is(statErr, os.ErrNotExist) {
							t.Fatalf("retention mismatch for %s: %v", root, statErr)
						}
					}
					if uncertain {
						if data, err := os.ReadFile(sourcePath); err != nil || string(data) != "input" {
							t.Fatal("retained input changed", string(data), err)
						}
					}
					if _, err := os.Lstat(filepath.Join(options.CacheDir, "host")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("failed execution published cache", err)
					}
					if result.OCI != "" || result.Filesystem != "" || result.Cache.Stored != 0 {
						t.Fatal("failed execution promoted outputs", result)
					}
					for _, name := range []string{"files", "files.oci"} {
						if _, err := os.Lstat(filepath.Join(taskDir, name)); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("failed output was published", name, err)
						}
					}
				})
			}
		}
	}
}
