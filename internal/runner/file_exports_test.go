package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cooprwire "github.com/strio-dev/coopr/api"
	coopr "github.com/strio-dev/stoke/internal/engine"
	"github.com/strio-dev/stoke/internal/workflow"
)

func writeFileExportReceipts(options coopr.Options) ([]cooprwire.ExportResult, error) {
	var receipts []cooprwire.ExportResult
	for _, export := range options.Output.Exports {
		if err := os.WriteFile(export.Destination, []byte("selected payload\n"), 0755); err != nil {
			return nil, err
		}
		receipts = append(receipts, cooprwire.ExportResult{State: export.State, Source: export.Source, Destination: export.Destination, Status: "passed"})
	}
	return receipts, nil
}

func TestFileExportAdmissionPrecedesSelectedTaskEffects(t *testing.T) {
	for _, selection := range []string{"separate-jobs", "matrix", "live-input", "other-live-input", "read-only-parent", "unselected", "ci-selected"} {
		t.Run(selection, func(t *testing.T) {
			w := fixture(t, `version: 1
tasks:
  a:
    image: scratch
    steps: [{export: {source: /out, destination: dist/out}}]
  b:
    image: scratch
    steps: [{export: {source: /out, destination: dist/out/report}}]
  matrix:
    image: scratch
    strategy: {matrix: {mode: [a, b]}}
    steps: [{export: {source: /out, destination: matrix/out}}]
  live:
    image: scratch
    cache: false
    mounts: [{type: host, source: .., target: /live}]
    steps: [{run: [check]}, {export: {source: /out, destination: live/out}}]
  observer:
    image: scratch
    cache: false
    mounts: [{type: host, source: .., target: /live}]
    steps: [{run: [check]}]
`)
			options := testOptions(t)
			targets := []string{"a", "b"}
			switch selection {
			case "matrix":
				targets = []string{"matrix"}
			case "live-input":
				targets = []string{"live"}
			case "other-live-input":
				targets = []string{"a", "observer"}
			case "unselected", "read-only-parent", "ci-selected":
				targets = []string{"a"}
			}
			if selection == "ci-selected" {
				if err := os.MkdirAll(filepath.Join(options.ContextDir, "dist", "out"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if selection == "read-only-parent" {
				if os.Geteuid() == 0 {
					t.Skip("effective root can write mode-0555 directories")
				}
				parent := filepath.Join(options.ContextDir, "dist")
				if err := os.Mkdir(parent, 0555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(parent, 0755) })
			}
			calls := 0
			execute := func(_ context.Context, _ coopr.Program, options coopr.Options) (coopr.Result, error) {
				calls++
				receipts, err := writeFileExportReceipts(options)
				return coopr.Result{Exports: receipts}, err
			}
			var report *Report
			var err error
			if selection == "ci-selected" {
				report, err = runCI(t.Context(), w, CIRequest{Targets: targets, Execute: targets}, options, fakeEngine{execute: execute})
			} else {
				report, err = runLiteral(t.Context(), w, targets, options, execute)
			}
			if selection == "unselected" {
				if err != nil || calls != 1 || report.Tasks[0].Exports[0].Status != "passed" {
					t.Fatalf("unselected collision blocked valid task: calls=%d report=%+v err=%v", calls, report, err)
				}
			} else {
				if err == nil || calls != 0 || report != nil {
					t.Fatalf("unsupported export layout reached effects: calls=%d report=%+v err=%v", calls, report, err)
				}
				if _, err := os.Stat(options.StateDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("collision created run state: %v", err)
				}
			}
		})
	}
}

func TestFileExportsPreserveFailedTaskAndNativePayload(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failed, ci bool
	}{{"passed", false, false}, {"failed", true, false}, {"ci-failed", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			failed := tc.failed
			w := fixture(t, `version: 1
tasks:
  check:
    image: scratch
    steps: [{export: {source: /out/tool, destination: dist/tool}}, {export: {source: /reports, destination: reports}}]
  gate:
    image: scratch
    needs: [check]
    steps: [{export: {source: /gate, destination: gate/report}}]
`)
			options := testOptions(t)
			if tc.ci {
				check := w.Tasks["check"]
				fresh := false
				check.Cache = &fresh
				w.Tasks["check"] = check
				options.CacheTo = []string{"registry.example.test/cache"}
			}
			original := errors.New("ordinary test exited 23")
			var before os.FileInfo
			var staging string
			calls := 0
			execute := func(_ context.Context, _ coopr.Program, native coopr.Options) (coopr.Result, error) {
				calls++
				if tc.ci && (native.Output.OCI != "" || native.Cache.LocalDir != "" || len(native.Cache.RegistryTo) != 0 || !native.Cache.NoCache) {
					t.Error("failed CI report requested unused OCI or portable writes")
				}
				if len(native.Output.Exports) == 1 {
					receipts, err := writeFileExportReceipts(native)
					return coopr.Result{ImageID: "gate", Exports: receipts}, err
				}
				if native.Output.Filesystem != "" || overlap(options.ContextDir, native.Output.Exports[0].Destination) {
					t.Error("selected export requested full rootfs or staged inside input context")
				}
				staging = filepath.Dir(native.Output.Exports[0].Destination)
				if err := os.WriteFile(native.Output.Exports[0].Destination, []byte("executable\n"), 0755); err != nil {
					return coopr.Result{}, err
				}
				if err := os.Chmod(native.Output.Exports[0].Destination, 0755); err != nil {
					return coopr.Result{}, err
				}
				var err error
				before, err = os.Stat(native.Output.Exports[0].Destination)
				if err != nil {
					return coopr.Result{}, err
				}
				if err := os.Mkdir(native.Output.Exports[1].Destination, 0750); err != nil {
					return coopr.Result{}, err
				}
				if err := os.WriteFile(filepath.Join(native.Output.Exports[1].Destination, "result"), []byte("report\n"), 0640); err != nil {
					return coopr.Result{}, err
				}
				if err := os.Symlink("result", filepath.Join(native.Output.Exports[1].Destination, "latest")); err != nil {
					return coopr.Result{}, err
				}
				var receipts []cooprwire.ExportResult
				for _, export := range native.Output.Exports {
					receipts = append(receipts, cooprwire.ExportResult{State: export.State, Source: export.Source, Destination: export.Destination, Status: "passed"})
				}
				result := coopr.Result{ImageID: "exact-image", ManifestDigest: "sha256:exact", Exports: receipts}
				if failed {
					return result, original
				}
				return result, nil
			}
			var report *Report
			var err error
			if tc.ci {
				report, err = runCI(t.Context(), w, CIRequest{Targets: []string{"gate"}, Execute: []string{"check"}}, options, fakeEngine{execute: execute})
			} else {
				report, err = runLiteral(t.Context(), w, []string{"gate"}, options, execute)
			}
			if errors.Is(err, original) != failed || !failed && err != nil || report == nil {
				t.Fatalf("task failure changed: %+v %v", report, err)
			}
			item := report.Tasks[0]
			if item.Status != map[bool]string{false: "passed", true: "failed"}[failed] || len(item.Exports) != 2 || item.Exports[0].Status != "passed" {
				t.Fatalf("report lost execution or file status: %+v", item)
			}
			after, err := os.Stat(filepath.Join(options.ContextDir, "dist/tool"))
			if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0755 {
				t.Fatalf("export was recopied or changed mode: %v %v", after, err)
			}
			if target, err := os.Readlink(filepath.Join(options.ContextDir, "reports/latest")); err != nil || target != "result" {
				t.Fatalf("admitted relative symlink changed: %q %v", target, err)
			}
			if failed && (calls != 1 || !tc.ci && (report.Tasks[1].Status != "skipped" || report.Tasks[1].Exports[0].Status != "skipped")) {
				t.Fatalf("diagnostic publication opened success gate: calls=%d tasks=%+v", calls, report.Tasks)
			}
			if tc.ci && (item.OCI != "" || item.Digest != "") {
				t.Fatal("failed CI report advertised an unrequested OCI output")
			}
			if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed task retained private staging: %v", err)
			}
		})
	}
}

func TestFileExportsRejectUnconfirmedOrChangedPublication(t *testing.T) {
	for _, mode := range []string{"canceled", "cleanup-failed", "raced", "symlink-parent", "nested-symlink-parent", "forged", "missing-receipt"} {
		t.Run(mode, func(t *testing.T) {
			w := fixture(t, `version: 1
tasks:
  check:
    image: scratch
    steps: [{export: {source: /report, destination: dist/report}}]
`)
			options := testOptions(t)
			if mode == "nested-symlink-parent" {
				w.Tasks["check"].Steps[0].Export.Destination = "dist/new/report"
			}
			destination := filepath.Join(options.ContextDir, w.Tasks["check"].Steps[0].Export.Destination)
			outside := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var staging string
			report, err := runLiteral(ctx, w, []string{"check"}, options, func(_ context.Context, _ coopr.Program, native coopr.Options) (coopr.Result, error) {
				staging = filepath.Dir(native.Output.Exports[0].Destination)
				t.Cleanup(func() { _ = os.RemoveAll(staging) })
				receipts, err := writeFileExportReceipts(native)
				if err != nil {
					return coopr.Result{}, err
				}
				switch mode {
				case "canceled":
					cancel()
					return coopr.Result{Exports: receipts}, ctx.Err()
				case "cleanup-failed":
					return coopr.Result{Exports: receipts}, coopr.ErrCleanupFailed
				case "raced":
					if err := os.Mkdir(filepath.Join(options.ContextDir, "dist"), 0755); err != nil {
						return coopr.Result{}, err
					}
					if err := os.WriteFile(filepath.Join(options.ContextDir, "dist/report"), []byte("keep\n"), 0600); err != nil {
						return coopr.Result{}, err
					}
				case "symlink-parent":
					if err := os.Symlink(outside, filepath.Join(options.ContextDir, "dist")); err != nil {
						return coopr.Result{}, err
					}
				case "nested-symlink-parent":
					if err := os.Mkdir(filepath.Join(options.ContextDir, ".git"), 0755); err != nil {
						return coopr.Result{}, err
					}
					if err := os.Symlink(".git", filepath.Join(options.ContextDir, "dist")); err != nil {
						return coopr.Result{}, err
					}
				case "forged":
					receipts[0].Destination = filepath.Join(outside, "report")
				case "missing-receipt":
					receipts = nil
				}
				return coopr.Result{Exports: receipts}, nil
			})
			if err == nil || report == nil || report.Tasks[0].Exports[0].Status == "passed" {
				t.Fatalf("unconfirmed publication succeeded: %+v %v", report, err)
			}
			if mode == "raced" {
				data, err := os.ReadFile(filepath.Join(options.ContextDir, "dist/report"))
				if err != nil || string(data) != "keep\n" {
					t.Fatalf("racing output replaced: %q %v", data, err)
				}
			} else if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe output appeared: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(options.ContextDir, ".git/new")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("publication created a directory through a reserved-path alias: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(outside, "report")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("publication escaped workspace: %v", err)
			}
			if mode == "cleanup-failed" {
				if !errors.Is(err, coopr.ErrCleanupFailed) {
					t.Fatalf("cleanup failure was lost: %v", err)
				}
				if _, err := os.Stat(staging); err != nil {
					t.Fatalf("unconfirmed worker staging removed: %v", err)
				}
			}
		})
	}
}

func TestFileExportsRequireCapabilityBeforeExecution(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  check:
    image: scratch
    steps: [{export: {source: /report, destination: report}}]
`)
	plan, err := w.Expand([]string{"check"})
	if err != nil {
		t.Fatal(err)
	}
	if err := testCapabilities(t, plan, cooprwire.Info{Capabilities: []string{"execute"}}, Options{}); err == nil || !strings.Contains(err.Error(), "selected-file-exports") {
		t.Fatalf("missing export capability admitted: %v", err)
	}
	if err := testCapabilities(t, plan, cooprwire.Info{Capabilities: []string{"execute", "selected-file-exports"}}, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestFileExportLoweringPreservesOperationPosition(t *testing.T) {
	w := fixture(t, `version: 1
tasks:
  build:
    image: scratch
    env: {MODE: stable}
    workdir: /work
    steps:
      - run: [produce]
      - export: {source: /out, destination: first}
      - export: {source: /out, destination: second}
      - run: [replace]
      - export: {source: /out, destination: third}
`)
	task := w.Tasks["build"]
	for _, following := range []bool{false, true} {
		current := task.Clone()
		if following {
			current.Steps = append(current.Steps, workflow.Step{Run: []string{"finish"}})
		}
		program, err := compile("build", current, nil)
		wantCount := 2
		if following {
			wantCount++
		}
		if err != nil || len(program.States) != wantCount {
			t.Fatalf("ordered states: %+v %v", program, err)
		}
		bindings := fileExportStates("build", current.Steps)
		if len(bindings) != 3 || bindings[0] != bindings[1] || bindings[0] != program.States[0].ID || bindings[2] != program.States[1].ID {
			t.Fatalf("export state bindings do not match operation position: %v %+v", bindings, program)
		}
		for i, state := range program.States {
			if i > 0 && state.From.State != program.States[i-1].ID {
				t.Fatalf("state %d lost its predecessor: %+v", i, state)
			}
			last := state.Operations[len(state.Operations)-1]
			if last.Kind != "run" || last.Args[0] != []string{"produce", "replace", "finish"}[i] {
				t.Fatalf("export changed the image operation order: %+v", state)
			}
		}
		if program.Output != program.States[len(program.States)-1].ID {
			t.Fatal("export changed final image selection")
		}
		changed := current.Clone()
		changed.Steps[1].Export.Source, changed.Steps[1].Export.Destination = "/different", "elsewhere"
		again, err := compile("build", changed, nil)
		if err != nil || !reflect.DeepEqual(program, again) {
			t.Fatalf("file selection changed image mutations: %+v %v", again, err)
		}
	}
}
