package artifact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type registryCacheReport = ciRegistryReport

// TestRegistryCacheWithCoopr qualifies registry transport using independent local
// stores. The fixture never contacts a registry outside this test process.
func TestRegistryCacheWithCoopr(t *testing.T) {
	// Unlike the CI entrypoints, this original host suite needs no explicit
	// evidence-parent setting. Honor the standard temporary directory selection.
	if os.Getenv("STOKE_CACHE_CI_EVIDENCE_DIR") == "" {
		t.Setenv("STOKE_CACHE_CI_EVIDENCE_DIR", os.TempDir())
	}
	f := newCIRegistryFixture(t)
	if os.Geteuid() == 0 {
		t.Fatal("registry cache acceptance requires a non-root single-user coordinator")
	}
	ctx, cancel := context.WithTimeout(f.ctx, 8*time.Minute)
	defer cancel()
	f.ctx = ctx
	source, refresh := f.registry+"/cache/source", f.registry+"/cache/refresh"
	writes := func(repository string) int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, request := range f.trace {
			if strings.HasPrefix(request["path"], "/v2/"+repository+"/") && request["method"] != "GET" && request["method"] != "HEAD" {
				n++
			}
		}
		return n
	}
	caller := filepath.Join(f.work, "caller")
	f.write(filepath.Join(caller, "busybox"), f.read(os.Getenv("STOKE_SMOKE_BUSYBOX")), 0755)
	payload := filepath.Join(caller, "payload.txt")
	f.write(payload, []byte("registry-payload\n"), 0644)
	f.write(filepath.Join(caller, "native.coopr"), []byte(`from "scratch" {
    env CACHE_BRANCH="native"
    copy "busybox" "/bin/busybox"
    copy "payload.txt" "/payload"
    run {
        exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox cp /payload /result.txt; echo STOKE_REGISTRY_NATIVE_RAN"
    }
}
`), 0644)
	f.write(filepath.Join(caller, "component.coopr"), []byte(`package as="payload" {
    copy "payload.txt" "/component-payload"
}
extend {
    copy "/component-payload" "/installed" from="payload"
}
`), 0644)
	workflow := filepath.Join(caller, "stoke.yaml")
	f.write(workflow, f.encode(map[string]any{"version": 1, "tasks": map[string]any{
		"literal": map[string]any{"image": "scratch", "env": map[string]string{"CACHE_BRANCH": "literal"}, "steps": []any{
			map[string]any{"copy": map[string]any{"sources": []string{"busybox"}, "destination": "/bin/busybox"}},
			map[string]any{"copy": map[string]any{"sources": []string{"payload.txt"}, "destination": "/payload"}},
			map[string]any{"run": []string{"/bin/busybox", "sh", "-eu", "-c", "/bin/busybox cp /payload /result.txt; echo STOKE_REGISTRY_LITERAL_RAN"}},
		}},
		"native":    map[string]any{"coopr": map[string]any{"files": []string{"native.coopr"}}},
		"component": map[string]any{"coopr": map[string]any{"kind": "component", "files": []string{"component.coopr"}}},
	}}), 0600)
	run := func(label, home, expected string, cacheFlags []string, success bool) (registryCacheReport, []byte) {
		t.Helper()
		report, log := f.runCommand(label, f.command(home, caller, workflow, append([]string{"--storage-driver=vfs"}, cacheFlags...)), success)
		if success {
			if len(report.Tasks) != 3 {
				t.Fatalf("%s report status/tasks: %+v", label, report)
			}
			for _, task := range report.Tasks {
				if task.Status != "passed" {
					t.Fatalf("%s task failed: %+v", label, task)
				}
				if task.Kind == "component" {
					if info, err := os.Stat(task.OCI); err != nil || info.Size() == 0 {
						t.Fatalf("%s missing component archive: %v", label, err)
					}
				} else if got := string(f.read(filepath.Join(task.Filesystem, "result.txt"))); got != expected {
					t.Fatalf("%s/%s result=%q want %q", label, task.Name, got, expected)
				}
				if task.Name != "literal" && task.Cache != nil {
					t.Fatal("native task fabricated unavailable cache counters")
				}
			}
		}
		return report, log
	}
	markers := []string{"STOKE_REGISTRY_LITERAL_RAN", "STOKE_REGISTRY_NATIVE_RAN"}
	producer, cold := run("producer", "producer", "registry-payload\n", []string{"--cache-to", source}, true)
	f.requireMarkers(cold, markers, true)
	f.pruneRealNative("producer", producer, &realCIExecution{driver: "vfs", pruneNative: true})
	if err := os.RemoveAll(filepath.Join(f.work, "producer")); err != nil {
		t.Fatal(err)
	}
	f.checks = append(f.checks, "producer-exports-cache-and-local-state-is-deleted")
	before := writes("cache/source")
	warm, warmLog := run("consumer", "consumer", "registry-payload\n", []string{"--cache-from", source}, true)
	f.requireMarkers(warmLog, markers, false)
	f.requireHits(warm, "literal")
	if writes("cache/source") != before {
		t.Fatal("read-only registry cache was modified")
	}
	f.checks = append(f.checks, "fresh-consumer-reuses-registry-without-writing-source")
	forced, forcedLog := run("no-cache", "forced", "registry-payload\n", []string{"--cache-from", source, "--cache-to", refresh, "--no-cache"}, true)
	f.requireMarkers(forcedLog, markers, true)
	for _, task := range forced.Tasks {
		if task.Cache != nil && task.Cache.Hits != 0 {
			t.Fatal("--no-cache reused portable results")
		}
	}
	if writes("cache/refresh") == 0 {
		t.Fatal("--no-cache failed to save fresh registry results")
	}
	f.checks = append(f.checks, "no-cache-forces-execution-and-still-writes-explicit-destination")
	if writes("cache/source") != before {
		t.Fatal("a consumer wrote to the read-only source repository")
	}
	f.closeRegistry()
	offline, offlineLog := run("offline-local", "consumer", "registry-payload\n", nil, true)
	f.requireMarkers(offlineLog, markers, false)
	f.requireHits(offline, "literal")
	f.checks = append(f.checks, "registry-hits-seed-local-cache-for-registry-unavailable-run")
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.NotFound(w, r)
		} else {
			http.Error(w, "forced export failure", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(reject.Close)
	_, failedLog := run("export-failure", "failed", "", []string{"--cache-to", strings.TrimPrefix(reject.URL, "http://") + "/cache/reject"}, false)
	if !strings.Contains(string(failedLog), "500") && !strings.Contains(string(failedLog), "forced export failure") {
		t.Fatalf("export failure cause missing: %s", failedLog)
	}
	f.checks = append(f.checks, "explicit-registry-export-failure-fails-workflow")
	t.Logf("Registry-cache acceptance passed: %s", f.work)
}
