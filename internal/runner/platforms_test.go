package runner

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestPlatformMatrixReachesProgramsAndPersistedResults(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  test:
    image: scratch
    platform: {matrix: target}
    strategy: {matrix: {target: [linux/amd64, linux/arm64]}}
    steps: [{run: [check]}]
`)
	report, err := runLiteral(t.Context(), w, nil, testOptions(t), func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
		if p.Platform != "linux/amd64" && p.Platform != "linux/arm64" {
			t.Errorf("matrix platform not forwarded: %+v", p)
		}
		return coopr.Result{ImageID: string(p.Output), Platform: p.Platform}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	saved := savedReport(t, report)
	if len(saved.Tasks) != 2 || saved.Tasks[0].Platform != "linux/amd64" || saved.Tasks[1].Platform != "linux/arm64" {
		t.Fatalf("platform results lost: %+v", saved.Tasks)
	}
}

func TestFromInheritsActualNativePlatformInsteadOfRequestedDefault(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  build:
    platform: linux/amd64
    coopr: {files: [image.coopr]}
  test: {from: build, steps: [{run: [check]}]}
`)
	report, err := run(t.Context(), w, nil, nativeOptions(t), fakeEngine{
		build: func(_ context.Context, b cooprwire.Build, component bool, _ coopr.Options) (coopr.Result, error) {
			if component || b.Platform != "linux/amd64" {
				t.Errorf("native default not forwarded: %+v", b)
			}
			// A native final FROM can explicitly override the build default.
			return coopr.Result{ImageID: "actual-arm64", Platform: "linux/arm64/v8"}, nil
		},
		execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			if p.Platform != "linux/arm64" || p.States[0].From.Image != "containers-storage:actual-arm64" {
				t.Errorf("consumer did not inherit actual image platform: %+v", p)
			}
			return coopr.Result{ImageID: "tested", Platform: "linux/arm64"}, nil
		},
	})
	if err != nil || len(report.Tasks) != 2 || report.Tasks[1].Platform != "linux/arm64" {
		t.Fatalf("native platform inheritance: report=%+v err=%v", report, err)
	}
}

func TestExplicitFromPlatformConflictBlocksExecutionAndDependent(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  build: {image: scratch, platform: linux/arm64}
  test: {from: build, platform: linux/amd64, steps: [{run: [check]}]}
  after: {image: scratch, needs: [test]}
`)
	var calls atomic.Int32
	report, err := runLiteral(t.Context(), w, nil, testOptions(t), func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
		calls.Add(1)
		if p.Output != "build" {
			t.Errorf("incompatible image was executed: %s", p.Output)
		}
		return coopr.Result{ImageID: "arm64-image", Platform: "linux/arm64"}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "conflicts") || calls.Load() != 1 || report.Tasks[1].Status != "failed" || report.Tasks[2].Status != "skipped" {
		t.Fatalf("conflict gates: calls=%d report=%+v err=%v", calls.Load(), report, err)
	}
}

func TestFromRequiresUsableProducerPlatform(t *testing.T) {
	for _, platform := range []string{"", "windows/amd64", "linux/amd64,linux/arm64"} {
		t.Run(platform, func(t *testing.T) {
			_, err := compile("test", workflow.Task{From: "build"}, map[string]coopr.Result{"build": {ImageID: "image", Platform: platform}})
			if err == nil || !strings.Contains(err.Error(), "valid image platform") {
				t.Fatalf("unknown producer platform silently defaulted: %q %v", platform, err)
			}
		})
	}
}

func TestRequestedPlatformCannotAcceptMislabeledEngineResult(t *testing.T) {
	for _, actual := range []string{"", "linux/amd64"} {
		t.Run(actual, func(t *testing.T) {
			w := fixture(t, "version: 1\ntasks:\n  test: {image: scratch, platform: linux/arm64}\n  after: {image: scratch, needs: [test]}\n")
			report, err := runLiteral(t.Context(), w, nil, testOptions(t), func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
				if p.Output != "test" {
					t.Error("invalid engine result admitted dependent")
				}
				return coopr.Result{ImageID: "image", Platform: actual}, nil
			})
			if err == nil || !strings.Contains(err.Error(), "platform") || report.Tasks[0].Status != "failed" || report.Tasks[1].Status != "skipped" || report.Tasks[0].Platform != actual {
				t.Fatalf("mislabeled result: %+v %v", report, err)
			}
		})
	}
}

func TestPlatformEnvironmentIdentitySeparatesTargetsAndNormalizesAliases(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  test:
    platform: {matrix: target}
    environment: {files: [image.coopr]}
    strategy: {matrix: {target: [linux/amd64, linux/arm64, linux/aarch64]}}
    steps: [{run: [check]}]
`)
	var builds, executions atomic.Int32
	report, err := run(t.Context(), w, nil, nativeOptions(t), fakeEngine{
		build: func(_ context.Context, b cooprwire.Build, _ bool, _ coopr.Options) (coopr.Result, error) {
			builds.Add(1)
			if b.Platform != "linux/amd64" && b.Platform != "linux/arm64" {
				t.Errorf("environment target not normalized: %+v", b)
			}
			return coopr.Result{ImageID: b.Platform, Platform: b.Platform}, nil
		},
		execute: func(_ context.Context, p coopr.Program, _ coopr.Options) (coopr.Result, error) {
			executions.Add(1)
			if p.States[0].From.Image != "containers-storage:"+p.Platform {
				t.Errorf("environment crossed platform boundary: %+v", p)
			}
			return coopr.Result{ImageID: string(p.Output), Platform: p.Platform}, nil
		},
	})
	if err != nil || builds.Load() != 2 || executions.Load() != 3 || len(report.Tasks) != 5 {
		t.Fatalf("environment identity: builds=%d executions=%d report=%+v err=%v", builds.Load(), executions.Load(), report, err)
	}
	if w.Tasks["test"].Platform.Matrix != "target" {
		t.Fatal("platform lowering mutated original definition")
	}
}

func TestNativePlatformCapabilityOnlyRequiredForSelectedNativePlatform(t *testing.T) {
	for _, tc := range []struct {
		name, task string
		requires   bool
	}{
		{"literal", "image: scratch, platform: linux/arm64", false},
		{"native-default", "coopr: {files: [image.coopr]}", false},
		{"native-platform", "coopr: {files: [image.coopr]}, platform: linux/arm64", true},
		{"component-platform", "coopr: {files: [component.coopr], kind: component}, platform: linux/arm64", true},
		{"inline-platform", "environment: {files: [image.coopr]}, platform: linux/arm64", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := fixture(t, "version: 1\ntasks:\n  test: {"+tc.task+"}\n")
			plan, _, err := prepare(t.Context(), w, nil, nativeOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			info := cooprwire.Info{Capabilities: []string{"execute", "build", "component"}}
			err = testCapabilities(t, plan, info, Options{})
			if tc.requires {
				if err == nil || !strings.Contains(err.Error(), "build-platform") {
					t.Fatalf("unsupported native platform accepted: %v", err)
				}
				info.Capabilities = append(info.Capabilities, "build-platform")
				if err := testCapabilities(t, plan, info, Options{}); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatalf("existing workflow unnecessarily requires new capability: %v", err)
			}
		})
	}
}
