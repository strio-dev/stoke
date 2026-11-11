package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
)

func TestInlineEnvironmentSharedNativeProducerAndImageInheritance(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  one:
    environment: {files: [image.coopr], target: tools, args: {MODE: debug}}
    env: {MODE: override}
    workdir: /work
    steps: [{run: [check]}]
  two:
    environment: {files: [image.coopr], target: tools, args: {MODE: debug}}
    steps: [{run: [check]}]
`)
	opts := nativeOptions(t)
	var builds, executions atomic.Int32
	report, err := run(t.Context(), w, nil, opts, fakeEngine{
		build: func(_ context.Context, b cooprwire.Build, component bool, o coopr.Options) (coopr.Result, error) {
			builds.Add(1)
			if component || b.Target != "tools" || b.Args["MODE"] != "debug" || o.Output.Filesystem != "" || b.Files[0] != filepath.Join(opts.ContextDir, "image.coopr") {
				t.Errorf("environment build lost native inputs: %+v %+v", b, o)
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: "native-environment"}, nil
		},
		execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			executions.Add(1)
			state := p.States[0]
			if state.From.Image != "containers-storage:native-environment" {
				t.Errorf("consumer did not inherit stored native image: %+v", state.From)
			}
			if p.Output == "one" {
				if !reflect.DeepEqual(state.Operations[0], coopr.Operation{Kind: "env", Name: "MODE", Value: "override"}) || !reflect.DeepEqual(state.Operations[1], coopr.Operation{Kind: "workdir", Path: "/work"}) {
					t.Error("task overrides do not follow native image inheritance")
				}
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: string(p.Output)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 1 || executions.Load() != 2 || len(report.Tasks) != 3 || !strings.HasPrefix(report.Tasks[0].Name, "stoke-environment-") {
		t.Fatalf("builds=%d executions=%d report=%+v", builds.Load(), executions.Load(), report.Tasks)
	}
	if w.Tasks["one"].Environment == nil || w.Tasks["one"].From != "" {
		t.Fatal("environment lowering mutated the caller workflow")
	}
}

func TestInlineEnvironmentKeepsDistinctPrerequisiteGates(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  one: {environment: {files: [image.coopr]}, steps: [{run: [check]}]}
  two: {environment: {files: [image.coopr]}, needs: [one], steps: [{run: [check]}]}
`)
	var builds, executions atomic.Int32
	_, err := run(t.Context(), w, nil, nativeOptions(t), fakeEngine{
		build: func(context.Context, cooprwire.Build, bool, coopr.Options) (coopr.Result, error) {
			n := builds.Add(1)
			if n == 2 && executions.Load() != 1 {
				t.Error("second environment bypassed its prerequisite")
			}
			return coopr.Result{Platform: "linux/amd64", ImageID: "environment"}, nil
		},
		execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			executions.Add(1)
			return coopr.Result{Platform: "linux/amd64", ImageID: string(p.Output)}, nil
		},
	})
	if err != nil || builds.Load() != 2 || executions.Load() != 2 {
		t.Fatalf("distinct gates merged or cyclic: builds=%d executions=%d err=%v", builds.Load(), executions.Load(), err)
	}
}

func TestFailedGateBlocksInlineEnvironmentAndConsumer(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  gate: {image: scratch}
  test: {environment: {files: [image.coopr]}, needs: [gate], steps: [{run: [check]}]}
`)
	failure := errors.New("required gate failed")
	report, err := run(t.Context(), w, nil, nativeOptions(t), fakeEngine{
		build: func(context.Context, cooprwire.Build, bool, coopr.Options) (coopr.Result, error) {
			t.Error("environment ran after a failed gate")
			return coopr.Result{}, nil
		},
		execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			if p.Output != "gate" {
				t.Error("consumer ran after a failed gate")
			}
			return coopr.Result{}, failure
		},
	})
	if !errors.Is(err, failure) || len(report.Tasks) != 3 || report.Tasks[1].Status != "skipped" || report.Tasks[2].Status != "skipped" {
		t.Fatalf("failure/report=%v %+v", err, report)
	}
}

func TestEnvironmentMatrixUsesOneEquivalentProducer(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  test:
    environment: {files: [image.coopr]}
    strategy: {matrix: {mode: [a, b]}}
    steps: [{run: [check]}]
`)
	var builds atomic.Int32
	_, err := run(t.Context(), w, nil, nativeOptions(t), fakeEngine{
		build: func(context.Context, cooprwire.Build, bool, coopr.Options) (coopr.Result, error) {
			builds.Add(1)
			return coopr.Result{Platform: "linux/amd64", ImageID: "environment"}, nil
		},
		execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			return coopr.Result{Platform: "linux/amd64", ImageID: string(p.Output)}, nil
		},
	})
	if err != nil || builds.Load() != 1 {
		t.Fatalf("equivalent matrix environment builds=%d err=%v", builds.Load(), err)
	}
}

func TestEnvironmentFilesUnavailableBeforeExecution(t *testing.T) {
	w := fixture(t, "version: 1\ntasks: {test: {environment: {files: [missing.coopr]}, steps: [{run: [check]}]}}\n")
	opts := testOptions(t)
	opts.CooprBinary = "/not/an/engine"
	if _, err := Run(t.Context(), w, nil, opts); err == nil || !strings.Contains(err.Error(), "environment files") {
		t.Fatalf("missing environment did not fail before engine startup: %v", err)
	}
	if _, err := os.Stat(opts.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid environment created state: %v", err)
	}
}
