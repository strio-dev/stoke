package runner

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestRegistryCacheForwardedToBuildsButNotPublication(t *testing.T) {
	for _, noCache := range []bool{false, true} {
		opts := nativeOptions(t)
		opts.NoCache = noCache
		opts.CacheFrom = []string{"registry.example/read-one", "registry.example/read-two"}
		opts.CacheTo = []string{"registry.example/write"}
		check := func(cache coopr.CacheOptions) {
			t.Helper()
			if cache.LocalDir != filepath.Join(opts.CacheDir, "results") || !reflect.DeepEqual(cache.RegistryFrom, opts.CacheFrom) || !reflect.DeepEqual(cache.RegistryTo, opts.CacheTo) {
				t.Errorf("local/registry cache settings lost: %#v", cache)
			}
		}
		_, err := run(context.Background(), fixture(t, nativeWorkflow), nil, opts, fakeEngine{
			build: func(_ context.Context, _ cooprwire.Build, component bool, o coopr.Options) (coopr.Result, error) {
				check(o.Cache)
				if o.Cache.NoCache != noCache {
					t.Error("native global cache policy lost")
				}
				if component {
					return coopr.Result{Reference: "sha256:component"}, nil
				}
				return coopr.Result{Platform: "linux/amd64", ImageID: "image"}, nil
			},
			execute: func(_ context.Context, _ coopr.Program, o coopr.Options) (coopr.Result, error) {
				check(o.Cache)
				if !o.Cache.NoCache {
					t.Error("cache:false task lost read bypass")
				}
				return coopr.Result{Platform: "linux/amd64", ImageID: "tested"}, nil
			},
			publish: func(_ context.Context, _ cooprwire.Publish, o coopr.Options) (coopr.Result, error) {
				if len(o.Cache.RegistryFrom) != 0 || len(o.Cache.RegistryTo) != 0 || !o.Cache.NoCache {
					t.Errorf("publication received build cache config: %#v", o.Cache)
				}
				return coopr.Result{}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidRegistryCacheFailsBeforeStartingEngineOrCreatingState(t *testing.T) {
	w := fixture(t, "version: 1\ntasks: {work: {image: scratch}}\n")
	for _, write := range []bool{false, true} {
		opts := testOptions(t)
		opts.CooprBinary = "/missing/coopr"
		if write {
			opts.CacheTo = []string{"registry.example/cache:tag"}
		} else {
			opts.CacheFrom = []string{"registry.example/cache:tag"}
		}
		if report, err := Run(context.Background(), w, nil, opts); err == nil || report != nil || !strings.Contains(err.Error(), "cache-") {
			t.Fatalf("invalid config started engine: report=%v err=%v", report, err)
		}
		if report, err := runLiteral(context.Background(), w, nil, opts, func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
			t.Error("invalid registry cache executed a task")
			return coopr.Result{}, nil
		}); err == nil || report != nil {
			t.Fatalf("invalid config created run: report=%v err=%v", report, err)
		}
		if _, err := os.Stat(opts.StateDir); !os.IsNotExist(err) {
			t.Fatalf("invalid config created run state: %v", err)
		}
	}
}

func TestRegistryCacheServiceHelper(t *testing.T) {
	if os.Getenv("STOKE_REGISTRY_CACHE_HELPER") != "1" {
		return
	}
	socket := ""
	for index, argument := range os.Args {
		if argument == "--socket" && index+1 < len(os.Args) {
			socket = os.Args[index+1]
		}
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(4)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/info" {
			capabilities := []string{"execute"}
			if os.Getenv("STOKE_REGISTRY_CACHE_SUPPORTED") == "1" {
				capabilities = append(capabilities, "registry-cache")
			}
			_ = json.NewEncoder(w).Encode(cooprwire.Info{APIVersion: 1, Capabilities: capabilities})
			return
		}
		var input cooprwire.Request
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data, _ := json.Marshal(input)
		_ = os.WriteFile(os.Getenv("STOKE_REGISTRY_CACHE_REQUEST"), data, 0600)
		_ = json.NewEncoder(w).Encode(cooprwire.Event{Type: "result", Result: &cooprwire.Result{ImageID: "built"}})
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

func TestRegistryCacheCapabilityIsRequiredBeforeAnyExecution(t *testing.T) {
	t.Setenv("STOKE_REGISTRY_CACHE_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	wrapper := filepath.Join(t.TempDir(), "coopr")
	if err := os.WriteFile(wrapper, []byte("#!"+shell+"\nexec "+quote(executable)+" -test.run=TestRegistryCacheServiceHelper -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w := fixture(t, "version: 1\ntasks: {work: {image: scratch}}\n")
	for _, tc := range []struct {
		name, from, to string
		supported      bool
		wantError      bool
	}{
		{"local-only-old-engine", "", "", false, false},
		{"read-old-engine", "registry.example/read", "", false, true},
		{"write-old-engine", "", "registry.example/write", false, true},
		{"registry-engine", "registry.example/read", "registry.example/write", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STOKE_REGISTRY_CACHE_SUPPORTED", map[bool]string{false: "0", true: "1"}[tc.supported])
			opts := testOptions(t)
			opts.CooprBinary = wrapper
			if tc.from != "" {
				opts.CacheFrom = []string{tc.from}
			}
			if tc.to != "" {
				opts.CacheTo = []string{tc.to}
			}
			record := filepath.Join(t.TempDir(), "request.json")
			t.Setenv("STOKE_REGISTRY_CACHE_REQUEST", record)
			report, err := Run(context.Background(), w, nil, opts)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "registry-cache") || report != nil {
					t.Fatalf("missing capability ignored: report=%v err=%v", report, err)
				}
				for _, path := range []string{record, opts.StateDir} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("unsupported feature made execution state at %s: %v", path, err)
					}
				}
				return
			}
			if err != nil || report.Status != "passed" {
				t.Fatalf("supported run failed: report=%v err=%v", report, err)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			var request cooprwire.Request
			if err := json.Unmarshal(data, &request); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(request.Options.Cache.RegistryFrom, opts.CacheFrom) || !reflect.DeepEqual(request.Options.Cache.RegistryTo, opts.CacheTo) {
				t.Fatalf("registry config did not reach supported engine: %#v", request.Options.Cache)
			}
		})
	}
}

// Exercise workflow preflight through the real SDK, rather than duplicating
// capability matching in a test fake.
func testCapabilities(t *testing.T, plan *workflow.ExecutionPlan, info cooprwire.Info, options Options) error {
	t.Helper()
	directory, err := os.MkdirTemp("", "stoke-capabilities-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	info.APIVersion = cooprwire.Version
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/info" {
			t.Errorf("preflight attempted execution: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(info)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client, err := coopr.NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return validateCapabilities(t.Context(), plan, client, options)
}
