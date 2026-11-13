package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cooprwire "github.com/strio-dev/coopr/api"
	"github.com/strio-dev/stoke/internal/ci"
)

type executionCleanupRecord struct {
	Marker string
	Input  string
}

// This private fake service proves protocol/error/lifetime integration. It does
// not claim native execution or reproduce a failing host temporary filesystem.
func TestExecutionCleanupAPIHelper(t *testing.T) {
	if os.Getenv("STOKE_CLEANUP_API_HELPER") != "1" {
		return
	}
	socket := ""
	for index, arg := range os.Args {
		if arg == "--socket" && index+1 < len(os.Args) {
			socket = os.Args[index+1]
		}
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			_ = json.NewEncoder(w).Encode(cooprwire.Info{APIVersion: cooprwire.Version, Version: "cleanup-fixture", Capabilities: []string{"execute", "build", "build-platform", "named-contexts", "publish", "publish-oci-layout"}})
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected fixture request", 500)
			return
		}
		var request cooprwire.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		directory, input := "", ""
		if request.Build != nil && len(request.Build.Files) == 1 {
			input = request.Build.Files[0]
			directory = filepath.Dir(input)
			if filepath.Base(directory) != "defs" || !strings.HasPrefix(filepath.Base(filepath.Dir(directory)), "stoke-component-") {
				http.Error(w, "definition did not come from loader-owned assets", 500)
				return
			}
		} else if request.Program != nil && len(request.Program.States) == 1 && strings.HasPrefix(request.Program.States[0].From.Image, "oci:") {
			directory = request.Options.ContextDir
			input = filepath.Join(directory, strings.TrimPrefix(request.Program.States[0].From.Image, "oci:"), "index.json")
		} else {
			http.Error(w, "unexpected fixture program", 500)
			return
		}
		marker := filepath.Join(directory, ".coopr-stage-fixture", ".coopr-build-worker-fixture", "request.json")
		if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if err := os.WriteFile(marker, []byte("owned incomplete-worker evidence"), 0o600); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		data, _ := json.Marshal(executionCleanupRecord{Marker: marker, Input: input})
		if err := os.WriteFile(os.Getenv("STOKE_CLEANUP_RECORD"), data, 0o600); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		code := os.Getenv("STOKE_CLEANUP_CODE")
		_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Error: &cooprwire.Error{Code: code, Message: "fixture terminal cleanup_failed diagnostic at " + marker}})
	})}
	go func() {
		parent := os.NewFile(3, "parent")
		_, _ = io.Copy(io.Discard, parent)
		_ = parent.Close()
		_ = server.Close()
	}()
	_ = server.Serve(listener)
	os.Exit(0)
}

func executionCleanupFixture(t *testing.T) (string, string, string) {
	t.Helper()
	// Keep the owned service's Unix socket path below the kernel limit.
	temporary, err := os.MkdirTemp("", "stoke-input-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temporary) })
	t.Setenv("TMPDIR", temporary)
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Chdir(root)
	if err := os.Mkdir("caller", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("defs", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("caller", "user-input"), []byte("caller owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("defs", "image.coopr"), []byte("from \"scratch\" {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := "version: 1\nassets: [defs/image.coopr]\ntasks:\n  build:\n    coopr: {files: [defs/image.coopr], files-from: component}\n"
	if err := os.WriteFile("stoke.yaml", []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	wrapper := filepath.Join(root, "fake-coopr")
	script := "#!" + shell + "\nexec " + quote(executable) + " -test.run=^TestExecutionCleanupAPIHelper$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "execution-record.json")
	t.Setenv("STOKE_CLEANUP_API_HELPER", "1")
	t.Setenv("STOKE_CLEANUP_RECORD", record)
	return root, wrapper, record
}

func TestCLIExecutionCleanupPreservesLoadedAssetsOnlyOnUnconfirmedCleanup(t *testing.T) {
	for _, path := range []string{"run", "ci"} {
		for _, code := range []string{"cleanup_failed", "execution_failed"} {
			t.Run(path+"/"+code, func(t *testing.T) {
				root, engine, recordPath := executionCleanupFixture(t)
				t.Setenv("STOKE_CLEANUP_CODE", code)
				args := []string{"run", "-f", "stoke.yaml", "--context", "caller", "--coopr-binary", engine}
				if path == "ci" {
					var out, diagnostics bytes.Buffer
					if exit := run([]string{"ci", "generate", "--context", "caller", "build"}, &out, &diagnostics); exit != 0 {
						t.Fatalf("generate exit%d: %s", exit, &diagnostics)
					}
					data, err := os.ReadFile(filepath.Join(".stoke", "ci-plan.json"))
					if err != nil {
						t.Fatal(err)
					}
					plan, err := ci.Decode(bytes.NewReader(data))
					if err != nil {
						t.Fatal(err)
					}
					scope, err := json.Marshal(testCIScope(plan.ID))
					if err != nil {
						t.Fatal(err)
					}
					args = []string{"ci", "run", "--plan", filepath.Join(".stoke", "ci-plan.json"), "--job", plan.Groups[0].Jobs[0].ID, "--scope", string(scope), "--attempt", "1", "--coopr-binary", engine}
				}
				var out, diagnostics bytes.Buffer
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if exit := runContext(ctx, args, &out, &diagnostics); exit != 1 || !strings.Contains(diagnostics.String(), "fixture terminal cleanup_failed diagnostic") {
					t.Fatalf("execution did not reach typed API failure: exit%d stdout=%s stderr=%s", exit, &out, &diagnostics)
				}
				var record executionCleanupRecord
				data, err := os.ReadFile(recordPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				for _, file := range []string{record.Marker, record.Input} {
					_, err := os.Stat(file)
					if code == "cleanup_failed" && err != nil {
						t.Errorf("execution input/evidence removed before confirmed cleanup: %s (%v)", file, err)
					}
					if code != "cleanup_failed" && !os.IsNotExist(err) {
						t.Errorf("ordinary failure retained loader-owned input: %s (%v)", file, err)
					}
				}
				if data, err := os.ReadFile(filepath.Join(root, "caller", "user-input")); err != nil || string(data) != "caller owned" {
					t.Fatalf("caller input changed: %q (%v)", data, err)
				}
			})
		}
	}
}

func TestCLIExecutionInputCleanupWithoutExecution(t *testing.T) {
	for _, command := range []string{"check", "ci"} {
		t.Run(command, func(t *testing.T) {
			root, _, record := executionCleanupFixture(t)
			t.Setenv("STOKE_CLEANUP_CODE", "cleanup_failed")
			args := []string{"check", "-f", "stoke.yaml", "--context", "caller"}
			if command == "ci" {
				args = []string{"ci", "generate", "--context", "caller", "build"}
			}
			var out, diagnostics bytes.Buffer
			if exit := run(args, &out, &diagnostics); exit != 0 {
				t.Fatalf("nonexecuting command failed: exit%d stdout=%s stderr=%s", exit, &out, &diagnostics)
			}
			directories, err := filepath.Glob(filepath.Join(os.TempDir(), "stoke-component-*"))
			if err != nil || len(directories) != 0 {
				t.Fatalf("nonexecution retained loaded assets: %v (%v)", directories, err)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("nonexecuting command reached engine: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "defs", "image.coopr")); err != nil {
				t.Fatalf("caller-owned asset source removed: %v", err)
			}
		})
	}
}
