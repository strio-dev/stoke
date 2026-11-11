package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/assets"
	"github.com/strio-dev/stoke/internal/workflow"
)

func closeLoaded(t *testing.T, result *Result) {
	t.Helper()
	t.Cleanup(func() {
		if err := result.Close(); err != nil {
			t.Error(err)
		}
	})
}

func TestCallerValidationIncludesUnselectedAndUnownedPackageTrees(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		t.Run(map[bool]string{false: "unselected-import", true: "wrapper-owner"}[wrapper], func(t *testing.T) {
			caller := t.TempDir()
			t.Setenv("TMPDIR", caller)
			child := pin("child")
			packaged := pin("packaged")
			definition := "version: 1\nassets: [helper.sh]\ntasks: {unused: {image: scratch}}\n"
			if wrapper {
				definition = "version: 1\nassets: [helper.sh]\nimports: {child: " + child + "}\ntasks: {run: {uses: child/check}}\n"
			}
			docs := map[string]*artifact.Document{
				packaged: {Data: []byte(definition), Files: map[string]assets.File{"helper.sh": {Data: []byte("package helper\n")}}},
				child:    {Data: []byte("version: 1\ntasks: {check: {image: scratch}}\n")},
			}
			file := local(t, "version: 1\nimports: {package: "+packaged+"}\ntasks: {copy: {image: scratch, steps: [{copy: {sources: [.], destination: /project}}]}}\n")
			result, err := loader(docs).Load(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = result.Close() }()
			if result.Workflow.Tasks["copy"].ComponentDir != "" {
				t.Fatal("caller task unexpectedly owns imported files")
			}
			if wrapper {
				for _, task := range result.Workflow.Tasks {
					if task.ComponentDir != "" {
						t.Fatal("wrapper assets should have no retained task owner in this fixture")
					}
				}
			}
			if len(result.owned) != 1 {
				t.Fatalf("allocated package trees: %v", result.owned)
			}
			if err := result.ValidateContext(caller); err == nil || !strings.Contains(err.Error(), "TMPDIR") {
				t.Fatalf("unselected package entered caller context: %v", err)
			}
			if err := result.Close(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(caller)
			// local() creates its test YAML directory within this TMPDIR; only
			// loader-owned stoke-component entries must have been removed.
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "stoke-component-") {
					t.Fatal("owned package staging survived")
				}
			}
		})
	}
}

func TestLocalDeclaredAssetsAreSnapshotSeparateFromCaller(t *testing.T) {
	path := local(t, `version: 1
assets: [definitions/image.coopr, scripts/check.sh]
tasks:
  build:
    coopr: {files: [definitions/image.coopr], files-from: component}
  check:
    image: scratch
    steps:
      - copy: {scope: component, sources: [scripts/check.sh], destination: /check.sh}
`)
	owner := filepath.Dir(path)
	for name, data := range map[string]string{
		"definitions/image.coopr": "from \"scratch\" {}\n",
		"scripts/check.sh":        "#!/bin/sh\necho component\n",
		"caller-only.txt":         "not declared",
	} {
		file := filepath.Join(owner, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	result, err := (Loader{}).Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, result)
	build := result.Workflow.Tasks["build"]
	if result.ContextDir != owner || build.ComponentDir == "" || build.ComponentDir == owner || result.Workflow.Tasks["check"].ComponentDir != build.ComponentDir {
		t.Fatalf("component/caller ownership %#v", result)
	}
	if _, err := os.Stat(filepath.Join(build.ComponentDir, "caller-only.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("undeclared caller file materialized: %v", err)
	}
	if err := os.WriteFile(filepath.Join(owner, "definitions/image.coopr"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	caller := t.TempDir()
	files, err := DefinitionFilesForTask(build, caller)
	if err != nil || !reflect.DeepEqual(files, []string{filepath.Join(build.ComponentDir, "definitions/image.coopr")}) {
		t.Fatalf("component definition resolution %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil || string(data) != "from \"scratch\" {}\n" {
		t.Fatalf("component snapshot changed: %q %v", data, err)
	}
	if err := result.Close(); err != nil {
		t.Fatal(err)
	}
	if err := result.Close(); err != nil {
		t.Fatal("repeated close:", err)
	}
	if _, err := os.Stat(build.ComponentDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned component directory survived close: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(owner, "caller-only.txt")); err != nil || string(data) != "not declared" {
		t.Fatalf("close changed caller content: %q %v", data, err)
	}
}

func TestImportedPackageOwnershipAndDeduplication(t *testing.T) {
	a, b := pin("package-a"), pin("package-b")
	library := []byte(`version: 1
assets: [scripts/check.sh]
tasks:
  check:
    image: scratch
    steps: [{copy: {scope: component, sources: [scripts/check.sh], destination: /check.sh}}]
`)
	calls := map[string]int{}
	l := Loader{Fetch: func(_ context.Context, ref string) (*artifact.Document, error) {
		calls[ref]++
		return &artifact.Document{Reference: ref, Data: library, Files: map[string]assets.File{"scripts/check.sh": {Data: []byte(ref), Executable: true}}}, nil
	}}
	path := local(t, "version: 1\nimports: {a: "+a+", again: "+a+", b: "+b+"}\ntasks:\n  first: {uses: a/check}\n  second: {uses: b/check}\n  repeated: {uses: again/check}\n  variant:\n    uses: a/check\n    strategy: {matrix: {mode: [debug, release]}}\n")
	result, err := l.Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, result)
	first := result.Workflow.Tasks["first"].ComponentDir
	second := result.Workflow.Tasks["second"].ComponentDir
	if first == "" || second == "" || first == second || result.Workflow.Tasks["repeated"].ComponentDir != first || calls[a] != 1 || calls[b] != 1 {
		t.Fatalf("package identity/materialization first=%q second=%q calls=%v", first, second, calls)
	}
	for dir, want := range map[string]string{first: a, second: b} {
		data, err := os.ReadFile(filepath.Join(dir, "scripts/check.sh"))
		if err != nil || string(data) != want {
			t.Fatalf("wrong package bytes: %q %v", data, err)
		}
	}
	plan, err := result.Workflow.Expand([]string{"variant"})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range plan.Groups[0].Jobs {
		if job.Definition.ComponentDir != first {
			t.Fatalf("matrix variant lost owner: %#v", job)
		}
	}
}

func TestRemoteAssetsMustMatchDeclaration(t *testing.T) {
	ref := pin("package")
	for _, tc := range []struct {
		name, declaration string
		files             map[string]assets.File
	}{
		{"missing", "assets: [check.sh]\n", nil},
		{"undeclared", "", map[string]assets.File{"check.sh": {Data: []byte("extra")}}},
		{"wrong-name", "assets: [check.sh]\n", map[string]assets.File{"other.sh": {}}},
		{"escape", "", map[string]assets.File{"../outside": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := &artifact.Document{Data: []byte("version: 1\n" + tc.declaration + "tasks: {run: {image: scratch}}\n"), Files: tc.files}
			if result, err := loader(map[string]*artifact.Document{ref: document}).Load(context.Background(), "oci://"+ref); err == nil {
				_ = result.Close()
				t.Fatal("accepted declaration/payload mismatch")
			}
		})
	}
}

func TestFailedLoadRemovesOnlyAllocatedTrees(t *testing.T) {
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	caller := local(t, "version: 1\nassets: [check.sh]\nimports: {missing: "+pin("missing")+"}\ntasks: {run: {image: scratch}}\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(caller), "check.sh"), []byte("caller-owned"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := (Loader{Fetch: func(context.Context, string) (*artifact.Document, error) { return nil, errors.New("fetch failed") }}).Load(context.Background(), caller)
	if err == nil || !strings.Contains(err.Error(), "fetch failed") {
		t.Fatalf("missing failure: %v", err)
	}
	entries, err := os.ReadDir(temporary)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "stoke-component-") {
			t.Fatalf("partial load left owned tree %s", entry.Name())
		}
	}
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(caller), "check.sh")); err != nil || string(data) != "caller-owned" {
		t.Fatalf("failed load removed caller files: %q %v", data, err)
	}
}

func TestPackageMaterializationMetadataStableAcrossLoads(t *testing.T) {
	ref := pin("package")
	l := loader(map[string]*artifact.Document{ref: {Data: []byte("version: 1\nassets: [scripts/check.sh]\ntasks: {run: {image: scratch}}\n"), Files: map[string]assets.File{"scripts/check.sh": {Data: []byte("echo stable\n"), Executable: true}}}})
	first, err := l.Load(context.Background(), "oci://"+ref)
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, first)
	second, err := l.Load(context.Background(), "oci://"+ref)
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, second)
	a, b := first.Workflow.Tasks["run"].ComponentDir, second.Workflow.Tasks["run"].ComponentDir
	if a == b {
		t.Fatal("loads unexpectedly share mutable trees")
	}
	for _, relative := range []string{".", "scripts", "scripts/check.sh"} {
		x, err := os.Stat(filepath.Join(a, relative))
		if err != nil {
			t.Fatal(err)
		}
		y, err := os.Stat(filepath.Join(b, relative))
		if err != nil {
			t.Fatal(err)
		}
		if x.Mode() != y.Mode() || !x.ModTime().Equal(time.Unix(0, 0)) || !y.ModTime().Equal(time.Unix(0, 0)) {
			t.Fatalf("unstable component metadata %s: %v %v", relative, x, y)
		}
	}
}

func TestCanceledLoadCleansOwnedAssets(t *testing.T) {
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	caller := local(t, "version: 1\nassets: [check.sh]\nimports: {child: "+pin("child")+"}\ntasks: {run: {image: scratch}}\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(caller), "check.sh"), []byte("caller-owned"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := Loader{Fetch: func(context.Context, string) (*artifact.Document, error) {
		cancel()
		return &artifact.Document{Reference: pin("child"), Data: []byte("version: 1\ntasks: {run: {image: scratch}}\n")}, nil
	}}
	if result, err := l.Load(ctx, caller); !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("canceled load result=%v error=%v", result, err)
	}
	entries, err := os.ReadDir(temporary)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "stoke-component-") {
			t.Fatalf("canceled load left owned tree %s", entry.Name())
		}
	}
	if _, err := os.Stat(caller); err != nil {
		t.Fatal("cancellation removed caller input:", err)
	}
}

func TestDeclaredNativeReferenceRelocationAndDependencies(t *testing.T) {
	original, delivered := pin("component"), "destination.test/component@sha256:"+strings.Repeat("b", 64)
	workflowRef := pin("workflow")
	data := "version: 1\ntasks:\n  native:\n    coopr:\n      files: [image.coopr]\n      args:\n        COMPONENT: {ref: " + original + "}\n        LITERAL: '" + original + "'\n  deliver:\n    publish: {from: native, destinations: ['" + original + "']}\n"
	result, err := loader(map[string]*artifact.Document{workflowRef: {Data: []byte(data), Dependencies: map[string]string{original: delivered}}}).Load(context.Background(), "oci://"+workflowRef)
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, result)
	args := result.Workflow.Tasks["native"].Coopr.Args
	if args["COMPONENT"].Ref != delivered || *args["LITERAL"].Value != original || result.Workflow.Tasks["deliver"].Publish.Destinations[0] != original {
		t.Fatal("declared reference or literal/output boundary changed")
	}
	w, err := workflow.Decode(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := Dependencies(w); !reflect.DeepEqual(got, []string{original}) {
		t.Fatalf("dependencies %v", got)
	}
	RewriteReferences(w, map[string]string{original: delivered})
	if w.Tasks["native"].Coopr.Args["COMPONENT"].Ref != delivered || *w.Tasks["native"].Coopr.Args["LITERAL"].Value != original || w.Tasks["deliver"].Publish.Destinations[0] != original {
		t.Fatal("rewrite changed literal/output fields")
	}
}

func TestDefinitionFilesForTaskKeepsContextSelectionExplicit(t *testing.T) {
	caller, component := t.TempDir(), t.TempDir()
	for _, dir := range []string{caller, component} {
		if err := os.WriteFile(filepath.Join(dir, "image.coopr"), []byte(dir), 0600); err != nil {
			t.Fatal(err)
		}
	}
	task := workflow.Task{Coopr: &workflow.CooprBuild{Files: []string{"image.coopr"}}, ComponentDir: component}
	got, err := DefinitionFilesForTask(task, caller)
	if err != nil || !reflect.DeepEqual(got, []string{filepath.Join(caller, "image.coopr")}) {
		t.Fatalf("default caller resolution %v %v", got, err)
	}
	task.Coopr.FilesFrom = "component"
	got, err = DefinitionFilesForTask(task, caller)
	if err != nil || !reflect.DeepEqual(got, []string{filepath.Join(component, "image.coopr")}) {
		t.Fatalf("component resolution %v %v", got, err)
	}
	task.ComponentDir = ""
	if _, err := DefinitionFilesForTask(task, caller); err == nil {
		t.Fatal("unresolved component root accepted")
	}
}
