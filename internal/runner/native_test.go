package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

type fakeEngine struct {
	execute executeFunc
	build   func(context.Context, cooprwire.Build, bool, coopr.Options) (coopr.Result, error)
	publish func(context.Context, cooprwire.Publish, coopr.Options) (coopr.Result, error)
}

func (f fakeEngine) Execute(ctx context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
	if f.execute == nil {
		return coopr.Result{}, errors.New("unexpected literal execution")
	}
	return f.execute(ctx, p, o)
}
func (f fakeEngine) Build(ctx context.Context, b cooprwire.Build, component bool, o coopr.Options) (coopr.Result, error) {
	if f.build == nil {
		return coopr.Result{}, errors.New("unexpected native build")
	}
	return f.build(ctx, b, component, o)
}
func (f fakeEngine) Publish(ctx context.Context, p cooprwire.Publish, o coopr.Options) (coopr.Result, error) {
	if f.publish == nil {
		return coopr.Result{}, errors.New("unexpected publication")
	}
	return f.publish(ctx, p, o)
}

func runLiteral(ctx context.Context, w *workflow.Workflow, targets []string, o Options, execute executeFunc) (*Report, error) {
	return run(ctx, w, targets, o, fakeEngine{execute: execute})
}

func nativeOptions(t *testing.T) Options {
	t.Helper()
	opts := testOptions(t)
	for _, name := range []string{"image.coopr", "component.coopr"} {
		if err := os.WriteFile(filepath.Join(opts.ContextDir, name), []byte("from \"scratch\" {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return opts
}

const nativeWorkflow = `version: 1
tasks:
  toolkit:
    coopr: {kind: component, files: [component.coopr]}
  build:
    coopr:
      files: [image.coopr]
      target: release
      args:
        COMPONENT: {from: toolkit}
        MESSAGE: 'literal ${value} "quoted"'
    outputs: {filesystem: false}
  test:
    from: build
    cache: false
    steps: [{run: [check]}]
  deliver-image:
    publish: {from: build, destinations: ['registry.invalid/image:test']}
    needs: [test]
  deliver-component:
    publish: {from: toolkit, destinations: ['registry.invalid/component:test']}
    needs: [test]
`

func TestNativeBuildComponentBindingsAndPublication(t *testing.T) {
	opts := nativeOptions(t)
	var calls []string
	var callsMu sync.Mutex
	record := func(name string) {
		callsMu.Lock()
		defer callsMu.Unlock()
		calls = append(calls, name)
	}
	var componentStore string
	engine := fakeEngine{
		build: func(_ context.Context, b cooprwire.Build, component bool, o coopr.Options) (coopr.Result, error) {
			if componentStore == "" {
				componentStore = b.ComponentStoreDir
			}
			if b.ComponentStoreDir != componentStore || !filepath.IsAbs(componentStore) || !filepath.IsAbs(b.Files[0]) {
				t.Error("native resources not resolved/shared")
			}
			if component {
				record("component")
				if o.Output.Filesystem != "" || !strings.HasSuffix(o.Output.OCI, "component.oci.tar") {
					t.Error("component output is not archive-only")
				}
				return coopr.Result{Reference: "sha256:component", OCI: o.Output.OCI, ManifestDigest: "sha256:export"}, nil
			}
			record("image")
			if b.Target != "release" || b.Args["COMPONENT"] != "sha256:component" || b.Args["MESSAGE"] != `literal ${value} "quoted"` || o.Output.Filesystem != "" {
				t.Errorf("native request: %#v %#v", b, o.Output)
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: "built-image", OCI: o.Output.OCI}, nil
		},
		execute: func(_ context.Context, p coopr.Program, o coopr.Options) (coopr.Result, error) {
			record("test")
			if p.States[0].From.Image != "containers-storage:built-image" || !o.Cache.NoCache {
				t.Error("test lost native image or cache policy")
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: "tested", CacheAvailable: true, Cache: coopr.CacheStats{Hits: 2}}, nil
		},
		publish: func(_ context.Context, p cooprwire.Publish, o coopr.Options) (coopr.Result, error) {
			record("publish-" + p.Artifact)
			if p.ComponentStoreDir != componentStore || o.Output != (coopr.Output{}) || !o.Cache.NoCache {
				t.Error("publication resources/cache policy")
			}
			if p.Artifact == "image" && p.Source != "built-image" || p.Artifact == "component" && p.Source != "sha256:component" {
				t.Errorf("publication source %#v", p)
			}
			return coopr.Result{Destinations: []cooprwire.Destination{{Destination: p.Destinations[0], Status: "succeeded"}}}, nil
		},
	}
	report, err := run(context.Background(), fixture(t, nativeWorkflow), []string{"deliver-image", "deliver-component"}, opts, engine)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 || !reflect.DeepEqual(calls[:3], []string{"component", "image", "test"}) {
		t.Fatalf("call order %v", calls)
	}
	publications := slices.Clone(calls[3:])
	slices.Sort(publications)
	if !reflect.DeepEqual(publications, []string{"publish-component", "publish-image"}) {
		t.Fatalf("publication calls %v", publications)
	}
	saved := savedReport(t, report)
	if saved.Tasks[0].Cache != nil || saved.Tasks[1].Cache != nil || saved.Tasks[2].Cache == nil || saved.Tasks[2].Cache.Hits != 2 {
		t.Fatal("native unknown cache counts fabricated or literal counts lost")
	}
	if saved.Tasks[0].Kind != "component" || saved.Tasks[0].Reference != "sha256:component" || len(saved.Tasks[4].Destinations) != 1 {
		t.Fatalf("artifact identities/publication lost: %#v", saved.Tasks)
	}
}

func TestNativeFailureGatesPublication(t *testing.T) {
	failure := errors.New("test assertion failed")
	pubs := 0
	engine := fakeEngine{
		build: func(_ context.Context, _ cooprwire.Build, component bool, _ coopr.Options) (coopr.Result, error) {
			if component {
				return coopr.Result{Reference: "sha256:component"}, nil
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: "image"}, nil
		},
		execute: func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
			return coopr.Result{}, failure
		},
		publish: func(context.Context, cooprwire.Publish, coopr.Options) (coopr.Result, error) {
			pubs++
			return coopr.Result{}, nil
		},
	}
	report, err := run(context.Background(), fixture(t, nativeWorkflow), nil, nativeOptions(t), engine)
	if !errors.Is(err, failure) || pubs != 0 || savedReport(t, report).Status != "failed" {
		t.Fatalf("publication gate pubs=%d err=%v report=%#v", pubs, err, report)
	}
}

func TestPartialPublicationRetainsDestinationResults(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  build: {image: scratch}
  publish:
    publish: {from: build, destinations: ['registry.invalid/a:test', 'registry.invalid/b:test']}
`)
	failure := errors.New("second destination failed")
	report, err := run(context.Background(), w, nil, testOptions(t), fakeEngine{
		execute: func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
			return coopr.Result{Platform: "linux/amd64", ImageID: "image"}, nil
		},
		publish: func(_ context.Context, p cooprwire.Publish, _ coopr.Options) (coopr.Result, error) {
			return coopr.Result{Destinations: []cooprwire.Destination{{Destination: p.Destinations[0], Status: "succeeded"}, {Destination: p.Destinations[1], Status: "failed", Error: failure.Error()}}, ProgressDropped: 9}, failure
		},
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	saved := savedReport(t, report)
	if saved.Tasks[1].Status != "failed" || len(saved.Tasks[1].Destinations) != 2 || saved.Tasks[1].Destinations[0].Status != "succeeded" || saved.Tasks[1].ProgressDropped != 9 {
		t.Fatalf("partial publication lost %#v", saved)
	}
}

func TestNativePreflightBeforeAnyExecution(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  first: {image: scratch}
  native: {coopr: {files: [missing.coopr]}, needs: [first]}
`)
	calls := 0
	engine := fakeEngine{execute: func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
		calls++
		return coopr.Result{Platform: "linux/amd64", ImageID: "first"}, nil
	}}
	if report, err := run(context.Background(), w, nil, testOptions(t), engine); err == nil || report != nil || calls != 0 {
		t.Fatalf("preflight report=%#v err=%v calls=%d", report, err, calls)
	}
	if _, err := run(context.Background(), w, []string{"first"}, testOptions(t), engine); err != nil || calls != 1 {
		t.Fatalf("unselected input blocked execution: %v", err)
	}
}

func TestNativeMatrixArgumentAndImageOutputBinding(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  base: {image: scratch}
  native:
    coopr:
      files: [image.coopr]
      args: {BASE: {from: base}, MODE: {matrix: mode}}
    strategy: {matrix: {mode: [debug, release]}, max-parallel: 1}
`)
	var modes []string
	_, err := run(context.Background(), w, nil, nativeOptions(t), fakeEngine{
		execute: func(context.Context, coopr.Program, coopr.Options) (coopr.Result, error) {
			return coopr.Result{Platform: "linux/amd64", ImageID: "base-image"}, nil
		},
		build: func(_ context.Context, b cooprwire.Build, _ bool, _ coopr.Options) (coopr.Result, error) {
			if b.Args["BASE"] != "containers-storage:base-image" {
				t.Errorf("base source %v", b.Args)
			}
			modes = append(modes, b.Args["MODE"])
			return coopr.Result{Platform: "linux/amd64", ImageID: b.Args["MODE"]}, nil
		},
	})
	if err != nil || !reflect.DeepEqual(modes, []string{"debug", "release"}) {
		t.Fatalf("matrix args %v error %v", modes, err)
	}
}
