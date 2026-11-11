package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	return Options{ContextDir: contextDir, StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "cache")}
}

func fixture(t *testing.T, data string) *workflow.Workflow {
	t.Helper()
	w, err := workflow.Decode(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func savedReport(t *testing.T, report *Report) Report {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(report.Directory, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Report
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestNeedsGateAndTaskOutputs(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  build:
    image: scratch
  test:
    from: build
    cache: false
    steps:
      - run: [check]
  artifact:
    image: scratch
    needs: [test]
    env: {Z_VALUE: '${literal}', A_VALUE: 'entrée "quoted"'}
    workdir: /work
    steps:
      - copy: {from: build, sources: [/app], destination: /app}
      - run: [check, '${literal}']
`)
	for _, globalNoCache := range []bool{false, true} {
		t.Run(map[bool]string{false: "task-policy", true: "global-policy"}[globalNoCache], func(t *testing.T) {
			opts := testOptions(t)
			opts.NoCache = globalNoCache
			var calls []string
			var store coopr.StoreOptions
			report, err := runLiteral(context.Background(), w, []string{"artifact"}, opts, func(_ context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
				name := string(p.Output)
				calls = append(calls, name)
				state := p.States[0]
				switch name {
				case "test":
					if state.From.Image != "containers-storage:build-image" {
						t.Errorf("test source %#v", state.From)
					}
				case "artifact":
					want := coopr.State{ID: "artifact", From: coopr.Source{Image: "scratch"}, Operations: []coopr.Operation{
						{Kind: "env", Name: "A_VALUE", Value: `entrée "quoted"`},
						{Kind: "env", Name: "Z_VALUE", Value: "${literal}"},
						{Kind: "workdir", Path: "/work"},
						{Kind: "copy", Sources: []string{"/app"}, Destination: "/app", From: coopr.Source{Image: "containers-storage:build-image"}},
						{Kind: "run", Args: []string{"check", "${literal}"}},
					}}
					if !reflect.DeepEqual(state, want) {
						t.Errorf("artifact lowering %#v; want %#v", state, want)
					}
				}
				if o.Cache.NoCache != (globalNoCache || name == "test") {
					t.Errorf("%s cache bypass=%v", name, o.Cache.NoCache)
				}
				if len(calls) == 1 {
					store = o.Store
				} else if !reflect.DeepEqual(store, o.Store) {
					t.Error("tasks do not share their run's store")
				}
				if !filepath.IsAbs(o.Output.OCI) || !filepath.IsAbs(o.Output.Filesystem) {
					t.Error("relative exports")
				}
				if _, err := os.Lstat(o.Output.OCI); !errors.Is(err, os.ErrNotExist) {
					t.Error("OCI destination must be fresh")
				}
				return coopr.Result{Platform: "linux/amd64", ImageID: name + "-image", ManifestDigest: "sha256:" + name, OCI: o.Output.OCI, Filesystem: o.Output.Filesystem, Cache: coopr.CacheStats{Hits: 1}, ProgressDropped: 7}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"build", "test", "artifact"}) {
				t.Fatalf("execution order %v", calls)
			}
			saved := savedReport(t, report)
			if saved.Status != "passed" || len(saved.Tasks) != 3 || saved.Tasks[1].Name != "test" || saved.Tasks[2].OCI != filepath.Join(report.Output, "artifact", "image") || saved.Tasks[2].ProgressDropped != 7 {
				t.Fatalf("saved report %#v", saved)
			}
		})
	}
}

func TestCancellationStopsScheduling(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  first: {image: scratch}
  second: {image: scratch, needs: [first]}
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	report, err := runLiteral(ctx, w, nil, testOptions(t), func(_ context.Context, _ coopr.Program, _ coopr.Options) (coopr.Result, error) {
		calls++
		cancel()
		return coopr.Result{Platform: "linux/amd64", ImageID: "first"}, nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	if savedReport(t, report).Status != "failed" {
		t.Fatal("canceled run marked passed")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	report, err = runLiteral(ctx, w, nil, testOptions(t), func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
		t.Fatal("canceled context executed")
		return coopr.Result{}, nil
	})
	if !errors.Is(err, context.Canceled) || report != nil {
		t.Fatalf("report=%v error=%v", report, err)
	}
}

func TestOutputOwnershipAndContextSeparation(t *testing.T) {
	w := fixture(t, "version: 1\ntasks:\n  build: {image: scratch}\n")
	for _, scenario := range []string{"existing-output", "output-in-context", "cache-symlink-in-context", "state-is-context-parent"} {
		t.Run(scenario, func(t *testing.T) {
			opts := testOptions(t)
			switch scenario {
			case "existing-output":
				opts.OutputDir = filepath.Join(filepath.Dir(opts.ContextDir), "output")
				if err := os.Mkdir(opts.OutputDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(opts.OutputDir, "keep"), []byte("user data"), 0600); err != nil {
					t.Fatal(err)
				}
			case "output-in-context":
				opts.OutputDir = filepath.Join(opts.ContextDir, "output")
			case "cache-symlink-in-context":
				alias := filepath.Join(filepath.Dir(opts.ContextDir), "alias")
				if err := os.Symlink(opts.ContextDir, alias); err != nil {
					t.Fatal(err)
				}
				opts.CacheDir = filepath.Join(alias, "cache")
			case "state-is-context-parent":
				opts.StateDir = filepath.Dir(opts.ContextDir)
			}
			_, err := runLiteral(context.Background(), w, nil, opts, func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
				t.Fatal("invalid ownership executed")
				return coopr.Result{}, nil
			})
			if err == nil {
				t.Fatal("invalid ownership accepted")
			}
			if scenario == "existing-output" {
				data, e := os.ReadFile(filepath.Join(opts.OutputDir, "keep"))
				if e != nil || string(data) != "user data" {
					t.Fatal("existing output changed")
				}
			}
		})
	}
}
