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

func TestCIProjectionExecutesClosedDataUnitWithoutExternalSuccessGates(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  gate: {image: scratch, cache: false}
  producer: {image: scratch, needs: [gate]}
  middle: {from: producer}
  selected: {from: middle, needs: [gate], cache: false}
  unrelated: {image: scratch}
`)
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cache-miss", true: "restored"}[warm], func(t *testing.T) {
			options := testOptions(t)
			options.Jobs = 1
			options.CacheFrom = []string{"registry.invalid/cache/read"}
			options.CacheTo = []string{"registry.invalid/cache/write"}
			var calls []string
			var store coopr.StoreOptions
			report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"producer", "middle", "selected"}}, options, fakeEngine{execute: func(_ context.Context, p coopr.Program, opts coopr.Options) (coopr.Result, error) {
				name := string(p.Output)
				calls = append(calls, name)
				if !reflect.DeepEqual(opts.Cache.RegistryFrom, options.CacheFrom) || !reflect.DeepEqual(opts.Cache.RegistryTo, options.CacheTo) {
					t.Errorf("cache settings were lost: %#v", opts.Cache)
				}
				if opts.Cache.NoCache != (name == "selected") {
					t.Errorf("%s cache policy changed: %#v", name, opts.Cache)
				}
				if len(calls) == 1 {
					store = opts.Store
				} else if !reflect.DeepEqual(store, opts.Store) {
					t.Error("data materialization did not share the consumer's native store")
				}
				want := map[string]string{"producer": "scratch", "middle": "containers-storage:producer-id", "selected": "containers-storage:middle-id"}[name]
				if p.States[0].From.Image != want {
					t.Errorf("%s input=%q want %q", name, p.States[0].From.Image, want)
				}
				stats := coopr.CacheStats{Misses: 1}
				if warm && name != "selected" {
					stats = coopr.CacheStats{Hits: 1}
				}
				return coopr.Result{ImageID: name + "-id", Platform: "linux/amd64", CacheAvailable: true, Cache: stats}, nil
			}})
			if err != nil || !reflect.DeepEqual(calls, []string{"producer", "middle", "selected"}) {
				t.Fatalf("projection calls=%v report=%#v error=%v", calls, report, err)
			}
			if report.Status != "passed" || len(report.Tasks) != 3 || savedReport(t, report).Status != "passed" {
				t.Fatalf("report=%#v", report)
			}
			if !reflect.DeepEqual(w.Tasks["producer"].Needs, []string{"gate"}) || !reflect.DeepEqual(w.Tasks["selected"].Needs, []string{"gate"}) {
				t.Fatal("CI projection mutated local workflow gates")
			}
		})
	}
}

func TestCIProjectionSelectsOneConcreteMatrixVariant(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  gate: {image: scratch, cache: false}
  checks:
    image: scratch
    needs: [gate]
    strategy: {matrix: {mode: [debug, release]}}
`)
	expanded, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	selected := expanded.Groups[1].Jobs[1].Name
	calls := []string{}
	report, err := runCI(t.Context(), w, CIRequest{Execute: []string{selected}}, testOptions(t), fakeEngine{execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
		calls = append(calls, string(p.Output))
		return coopr.Result{Platform: "linux/amd64"}, nil
	}})
	if err != nil || !reflect.DeepEqual(calls, []string{selected}) || len(report.Tasks) != 1 || report.Tasks[0].Matrix["mode"] != "release" {
		t.Fatalf("calls=%v report=%#v error=%v", calls, report, err)
	}
}

func TestCIProjectionKeepsFreshProducerAndInternalMatrixGate(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  fresh: {image: scratch, cache: false}
  checks:
    image: scratch
    needs: [fresh]
    strategy: {matrix: {mode: [first, second]}, fail-fast: false}
  left: {from: fresh, needs: [checks]}
  right: {from: fresh, needs: [checks]}
`)
	expanded, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	var selected []string
	for _, group := range expanded.Groups {
		for _, job := range group.Jobs {
			selected = append(selected, job.Name)
		}
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failed-gate"}[fail], func(t *testing.T) {
			var mu sync.Mutex
			counts := map[string]int{}
			checksComplete := 0
			failure := errors.New("test failed")
			report, err := runCI(t.Context(), w, CIRequest{Execute: selected}, testOptions(t), fakeEngine{execute: func(_ context.Context, p coopr.Program, opts coopr.Options) (coopr.Result, error) {
				mu.Lock()
				defer mu.Unlock()
				name := string(p.Output)
				counts[name]++
				if name == "fresh" && !opts.Cache.NoCache {
					t.Error("fresh producer used cached execution")
				}
				if strings.HasPrefix(name, "checks--matrix-") {
					checksComplete++
					if fail && checksComplete == 1 {
						return coopr.Result{}, failure
					}
				}
				if (name == "left" || name == "right") && checksComplete != 2 {
					t.Error("consumer bypassed all-variant internal gate")
				}
				return coopr.Result{ImageID: name + "-id", Platform: "linux/amd64"}, nil
			}})
			if counts["fresh"] != 1 || checksComplete != 2 {
				t.Fatalf("fresh producer/siblings repeated or skipped: %v", counts)
			}
			if fail {
				if !errors.Is(err, failure) || counts["left"] != 0 || counts["right"] != 0 || report.Status != "failed" {
					t.Fatalf("failed gate bypassed: counts=%v report=%#v error=%v", counts, report, err)
				}
			} else if err != nil || counts["left"] != 1 || counts["right"] != 1 || report.Status != "passed" {
				t.Fatalf("counts=%v report=%#v error=%v", counts, report, err)
			}
		})
	}
}

func TestCIProjectionRejectsExternalDataAndInvalidSelectionsBeforeExecution(t *testing.T) {
	for _, scenario := range []string{"cacheable", "fresh", "context-write", "no-cache", "missing", "duplicate", "empty", "partial-gate"} {
		t.Run(scenario, func(t *testing.T) {
			w := fixture(t, "version: 1\ntasks:\n  producer: {image: scratch}\n  consumer: {from: producer}\n")
			options := testOptions(t)
			selected := []string{"consumer"}
			producer := w.Tasks["producer"]
			switch scenario {
			case "fresh":
				value := false
				producer.Cache = &value
			case "context-write":
				value := false
				producer.Steps = []workflow.Step{{Run: []string{"write"}}}
				producer.Mounts = []workflow.Mount{{Type: "context", Source: ".", Target: "/work", ReadOnly: &value}}
			case "no-cache":
				options.NoCache = true
			case "missing":
				selected = []string{"missing"}
			case "duplicate":
				selected = append(selected, "consumer")
			case "empty":
				selected = nil
			case "partial-gate":
				w = fixture(t, "version: 1\ntasks:\n  checks: {image: scratch, strategy: {matrix: {mode: [one, two]}}}\n  consumer: {image: scratch, needs: [checks]}\n")
				expanded, err := w.Expand(nil)
				if err != nil {
					t.Fatal(err)
				}
				selected = []string{expanded.Groups[0].Jobs[0].Name, "consumer"}
			}
			if scenario != "partial-gate" {
				w.Tasks["producer"] = producer
			}
			report, err := runCI(t.Context(), w, CIRequest{Execute: selected}, options, fakeEngine{})
			if err == nil || report != nil {
				t.Fatalf("unsafe projection executed: report=%#v error=%v", report, err)
			}
			if _, statErr := os.Stat(options.StateDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid projection created runtime state: %v", statErr)
			}
		})
	}
}

func TestCIProjectionPrunesOnlyUnusedFreshImageOutputs(t *testing.T) {
	for _, mode := range []string{"gate", "required-value", "colocated-value", "terminal", "all-targets", "global-no-cache", "filesystem"} {
		t.Run(mode, func(t *testing.T) {
			w := fixture(t, "version: 1\ntasks:\n  check: {image: scratch, cache: false, outputs: {filesystem: false}, mounts: [{type: cache, id: checks, target: /cache}], steps: [{run: [check]}]}\n  finish: {image: scratch, needs: [check], outputs: {filesystem: false}}\n")
			options := testOptions(t)
			options.CacheFrom = []string{"registry.example.test/cache"}
			options.CacheTo = []string{"registry.example.test/cache"}
			request := CIRequest{Targets: []string{"finish"}, Execute: []string{"check"}}
			check := w.Tasks["check"]
			switch mode {
			case "required-value":
				request.RequiredOutputs = []string{"check"}
			case "colocated-value":
				finish := w.Tasks["finish"]
				finish.Image = ""
				finish.From = "check"
				w.Tasks["finish"] = finish
				request.Execute = append(request.Execute, "finish")
			case "terminal":
				request.Targets = []string{"check"}
			case "all-targets":
				request.Targets = nil
			case "global-no-cache":
				check.Cache, options.NoCache = nil, true
			case "filesystem":
				check.Outputs = nil
			}
			w.Tasks["check"] = check
			if mode == "gate" {
				plan, resolved, err := prepareCI(t.Context(), w, request, options)
				if err != nil {
					t.Fatal(err)
				}
				info := cooprwire.Info{Capabilities: []string{"execute", "run-mounts", "registry-cache"}}
				if err := testCapabilities(t, plan, info, resolved); err == nil || !strings.Contains(err.Error(), "optional-oci-output") {
					t.Fatalf("older engine admitted output pruning: %v", err)
				}
				info.Capabilities = append(info.Capabilities, "optional-oci-output")
				if err := testCapabilities(t, plan, info, resolved); err != nil {
					t.Fatal(err)
				}
			}
			report, err := runCI(t.Context(), w, request, options, fakeEngine{execute: func(_ context.Context, p coopr.Program, native coopr.Options) (coopr.Result, error) {
				pruned := mode == "gate" && p.Output == "check"
				if (native.Output.OCI == "") != pruned || (native.Cache.LocalDir == "") != pruned || (len(native.Cache.RegistryTo) == 0) != pruned {
					t.Errorf("%s output/cache demand changed: output=%+v cache=%+v", p.Output, native.Output, native.Cache)
				}
				if p.Output == "check" && !native.Cache.NoCache {
					t.Error("fresh gate lost its execution bypass")
				}
				if !reflect.DeepEqual(native.Cache.RegistryFrom, options.CacheFrom) || p.Output == "check" && native.Cache.MountDir == "" {
					t.Error("output pruning removed cache reads or mount acceleration")
				}
				if p.Output == "finish" && p.States[0].From.Image != "containers-storage:check-image" {
					t.Error("colocated consumer lost its native producer")
				}
				return coopr.Result{Platform: "linux/amd64", ImageID: string(p.Output) + "-image", ManifestDigest: "sha256:native", OCI: native.Output.OCI, Filesystem: native.Output.Filesystem}, nil
			}})
			if err != nil || report == nil || report.Status != "passed" || (report.Tasks[0].OCI == "") != (mode == "gate") {
				t.Fatalf("CI output report: %+v %v", report, err)
			}
			if mode == "gate" && report.Tasks[0].Digest != "" {
				t.Fatal("native identity advertised as an exported OCI digest")
			}
		})
	}
}

func TestCIProjectionClosedUnitsPreserveFreshAndEffectPolicies(t *testing.T) {
	for _, policy := range []string{"cacheable", "fresh", "writable-context", "forced-fresh"} {
		t.Run(policy, func(t *testing.T) {
			w := fixture(t, "version: 1\ntasks:\n  producer: {image: scratch}\n  consumer: {from: producer}\n")
			options := testOptions(t)
			options.Jobs = 1
			producer := w.Tasks["producer"]
			if policy == "fresh" {
				value := false
				producer.Cache = &value
			}
			if policy == "writable-context" {
				value := false
				producer.Steps = []workflow.Step{{Run: []string{"write"}}}
				producer.Mounts = []workflow.Mount{{Type: "context", Source: ".", Target: "/work", ReadOnly: &value}}
			}
			options.NoCache = policy == "forced-fresh"
			w.Tasks["producer"] = producer
			var calls []string
			report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"producer", "consumer"}}, options, fakeEngine{execute: func(_ context.Context, p coopr.Program, opts coopr.Options) (coopr.Result, error) {
				name := string(p.Output)
				calls = append(calls, name)
				if opts.Cache.NoCache != (options.NoCache || name == "producer" && policy == "fresh") {
					t.Error("cache/fresh policy changed")
				}
				return coopr.Result{ImageID: name, Platform: "linux/amd64"}, nil
			}})
			if err != nil || !reflect.DeepEqual(calls, []string{"producer", "consumer"}) || report.Status != "passed" {
				t.Fatalf("calls=%v report=%#v error=%v", calls, report, err)
			}
		})
	}
}

func TestCIProjectionRejectsMissingTransitiveDataBeforeExecution(t *testing.T) {
	w := fixture(t, "version: 1\ntasks:\n  base: {image: scratch}\n  middle: {from: base}\n  selected: {from: middle}\n")
	options := testOptions(t)
	report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"middle", "selected"}}, options, fakeEngine{})
	if err == nil || report != nil || !strings.Contains(err.Error(), "data-closed") || !strings.Contains(err.Error(), "base") {
		t.Fatalf("partial data chain accepted: report=%#v error=%v", report, err)
	}
}

func TestCIProjectionRejectsPartialUnpromotedSharedEnvironment(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  gate: {image: scratch, cache: false}
  checks:
    environment: {files: [image.coopr], target: tools}
    needs: [gate]
    cache: false
    strategy: {matrix: {mode: [first, second]}}
    steps: [{run: [check]}]
`)
	expanded, err := w.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := runCI(t.Context(), w, CIRequest{Execute: []string{expanded.Groups[1].Jobs[0].Name}}, nativeOptions(t), fakeEngine{})
	if err == nil || report != nil || !strings.Contains(err.Error(), "shared inline environment") {
		t.Fatalf("unbound shared environment rebuilt: report=%#v err=%v", report, err)
	}
}

func TestCIProjectionIncludesInlineEnvironmentDataArguments(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  base: {image: scratch}
  checks:
    environment: {files: [image.coopr], args: {BASE: {from: base}}}
    steps: [{run: [check]}]
`)
	builds, executions := 0, 0
	report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"base", "checks"}}, nativeOptions(t), fakeEngine{
		build: func(_ context.Context, b cooprwire.Build, _ bool, _ coopr.Options) (coopr.Result, error) {
			builds++
			if b.Args["BASE"] != "containers-storage:base" {
				t.Fatalf("environment data input missing: %#v", b)
			}
			return coopr.Result{ImageID: "environment", Platform: "linux/amd64"}, nil
		},
		execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			executions++
			return coopr.Result{ImageID: string(p.Output), Platform: "linux/amd64"}, nil
		},
	})
	if err != nil || builds != 1 || executions != 2 || len(report.Tasks) != 3 {
		t.Fatalf("builds=%d executions=%d report=%#v error=%v", builds, executions, report, err)
	}
}

func TestCIProjectionRejectsNonportablePathsBeforeExecution(t *testing.T) {
	for _, scenario := range []string{"host", "absolute-context", "parent-context", "context-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			w := fixture(t, "version: 1\ntasks: {selected: {image: scratch}}\n")
			options := testOptions(t)
			job := w.Tasks["selected"]
			switch scenario {
			case "host":
				job.Steps = []workflow.Step{{Run: []string{"check"}}}
				job.Mounts = []workflow.Mount{{Type: "host", Source: options.ContextDir, Target: "/live"}}
			case "absolute-context":
				job.Contexts = map[string]string{"outside": t.TempDir()}
			case "parent-context":
				job.Contexts = map[string]string{"outside": "../outside"}
			case "context-symlink":
				outside := t.TempDir()
				if err := os.Symlink(outside, filepath.Join(options.ContextDir, "linked")); err != nil {
					t.Fatal(err)
				}
				job.Contexts = map[string]string{"outside": "linked"}
			}
			w.Tasks["selected"] = job
			if report, err := runCI(t.Context(), w, CIRequest{Execute: []string{"selected"}}, options, fakeEngine{}); err == nil || report != nil {
				t.Fatalf("nonportable input accepted: %#v %v", report, err)
			}
		})
	}
}

func TestCIProjectionCancellationAndFailurePersistWithoutSuccessReceipts(t *testing.T) {
	w := fixture(t, "version: 1\ntasks: {selected: {image: scratch}}\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	returned := false
	report, err := runCI(ctx, w, CIRequest{Execute: []string{"selected"}}, testOptions(t), fakeEngine{execute: func(ctx context.Context, _ coopr.Program, _ coopr.Options) (coopr.Result, error) {
		cancel()
		<-ctx.Done()
		returned = true
		return coopr.Result{}, ctx.Err()
	}})
	if !errors.Is(err, context.Canceled) || !returned || report.Status != "failed" || savedReport(t, report).Status != "failed" {
		t.Fatalf("cancellation not drained/persisted: report=%#v error=%v", report, err)
	}
}

func TestOrdinaryRunStillExecutesCompleteNeedsClosure(t *testing.T) {
	w := fixture(t, "version: 1\ntasks:\n  fresh: {image: scratch, cache: false}\n  selected: {image: scratch, needs: [fresh]}\n")
	var calls []string
	report, err := runLiteral(t.Context(), w, []string{"selected"}, testOptions(t), func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
		calls = append(calls, string(p.Output))
		return coopr.Result{Platform: "linux/amd64"}, nil
	})
	if err != nil || !slices.Equal(calls, []string{"fresh", "selected"}) || len(report.Tasks) != 2 {
		t.Fatalf("ordinary Run changed: calls=%v report=%#v error=%v", calls, report, err)
	}
}

func TestCIProjectionPreservesDistinctEnvironmentGatesInDataUnit(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  ga: {image: scratch, cache: false}
  gb: {image: scratch, cache: false}
  p: {image: scratch}
  a:
    environment: {files: [image.coopr]}
    needs: [ga]
    cache: false
    mounts: [{type: stage, from: p, target: /input}]
    steps: [{run: [check]}]
  b:
    environment: {files: [image.coopr]}
    needs: [gb]
    cache: false
    mounts: [{type: stage, from: p, target: /input}]
    steps: [{run: [check]}]
`)
	for _, projection := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "projection"}[projection], func(t *testing.T) {
			options := nativeOptions(t)
			options.Jobs = 1
			builds := 0
			calls := map[string]int{}
			engine := fakeEngine{
				build: func(_ context.Context, _ cooprwire.Build, _ bool, opts coopr.Options) (coopr.Result, error) {
					builds++
					if !opts.Cache.NoCache {
						t.Error("fresh environment lost cache:false")
					}
					return coopr.Result{ImageID: "environment", Platform: "linux/amd64"}, nil
				},
				execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
					calls[string(p.Output)]++
					return coopr.Result{ImageID: string(p.Output), Platform: "linux/amd64"}, nil
				},
			}
			var report *Report
			var err error
			if projection {
				report, err = runCI(t.Context(), w, CIRequest{Execute: []string{"p", "a", "b"}}, options, engine)
			} else {
				report, err = run(t.Context(), w, nil, options, engine)
			}
			if err != nil {
				t.Fatal(err)
			}
			if builds != 2 {
				t.Fatalf("distinct authored environment gates merged: builds=%d report=%#v", builds, report.Tasks)
			}
			for _, name := range []string{"p", "a", "b"} {
				if calls[name] != 1 {
					t.Fatalf("%s executions=%d", name, calls[name])
				}
			}
			wantGates := 1
			if projection {
				wantGates = 0
			}
			if calls["ga"] != wantGates || calls["gb"] != wantGates {
				t.Fatalf("external gates: calls=%v", calls)
			}
		})
	}
}
