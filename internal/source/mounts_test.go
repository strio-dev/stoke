package source

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/stoke/internal/artifact"
	"github.com/strio-dev/stoke/internal/workflow"
)

func TestImportedMountsAndEnvironmentOwnDependencyScopes(t *testing.T) {
	ref := pin("environments")
	image := "source.test/tool@sha256:" + strings.Repeat("a", 64)
	files := "source.test/files@sha256:" + strings.Repeat("b", 64)
	movedImage := "destination.test/tool@sha256:" + strings.Repeat("c", 64)
	movedFiles := "destination.test/files@sha256:" + strings.Repeat("d", 64)
	library := `version: 1
inputs: {version: {required: true}}
tasks:
  base: {image: scratch}
  verify:
    environment:
      files: [ci.coopr]
      args: {BASE: {from: base}, VERSION: {input: version}, TOOL: {ref: "` + image + `"}}
    contexts: {fixtures: ./fixtures}
    mounts:
      - {type: stage, from: base, source: /bin, target: /bin-from-base, read-only: true}
      - {type: context, context: fixtures, target: /fixtures}
    steps:
      - run: [check]
        mounts:
          - {type: image, image: "` + image + `", target: /tools}
          - {type: artifact, ref: "` + files + `", target: /files}
`
	root := "version: 1\nimports: {tools: " + ref + `}
tasks:
  first: {uses: tools/verify, with: {version: first}}
  second: {uses: tools/verify, with: {version: second}}
`
	result, err := loader(map[string]*artifact.Document{ref: {
		Data: []byte(library), Dependencies: map[string]string{image: movedImage, files: movedFiles},
	}}).Load(context.Background(), local(t, root))
	if err != nil {
		t.Fatal(err)
	}
	closeLoaded(t, result)
	for _, name := range []string{"first", "second"} {
		task := result.Workflow.Tasks[name]
		base := name + "--tools--base"
		if task.Environment.Args["BASE"].From != base || task.Mounts[0].From != base || *task.Environment.Args["VERSION"].Value != name {
			t.Fatalf("%s lost invocation scope: %+v", name, task)
		}
		if task.Environment.Args["TOOL"].Ref != movedImage || task.Steps[0].Mounts[0].Image != movedImage || task.Steps[0].Mounts[1].Ref != movedFiles {
			t.Fatalf("%s lost verified relocation: %+v", name, task)
		}
	}
	first := result.Workflow.Tasks["first"]
	first.Environment.Files[0] = "changed.coopr"
	*first.Environment.Args["VERSION"].Value = "changed"
	first.Contexts["fixtures"] = "changed"
	*first.Mounts[0].ReadOnly = false
	first.Steps[0].Mounts[0].Image = "changed"
	second := result.Workflow.Tasks["second"]
	if second.Environment.Files[0] != "ci.coopr" || *second.Environment.Args["VERSION"].Value != "second" || second.Contexts["fixtures"] != "./fixtures" || !second.Mounts[0].IsReadOnly() || second.Steps[0].Mounts[0].Image != movedImage {
		t.Fatal("expanded mount/environment fields alias sibling template invocations")
	}
}

func TestEnvironmentAndMountRegistryInputsDeclareAndRewrite(t *testing.T) {
	image := "registry.test/tool@sha256:" + strings.Repeat("a", 64)
	files := "registry.test/files@sha256:" + strings.Repeat("b", 64)
	w, err := workflow.Decode(strings.NewReader(`version: 1
tasks:
  test:
    environment: {files: [ci.coopr], args: {TOOL: {ref: "` + image + `"}, LITERAL: "registry.test/undeclared:latest"}}
    mounts: [{type: image, image: "` + image + `", target: /tool}]
    steps:
      - run: [check]
        mounts: [{type: artifact, ref: "` + files + `", target: /files}]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{files, image}
	if got := Dependencies(w); !reflect.DeepEqual(got, want) {
		t.Fatalf("declared inputs = %v, want %v", got, want)
	}
	movedImage := "moved.test/tool@sha256:" + strings.Repeat("c", 64)
	movedFiles := "moved.test/files@sha256:" + strings.Repeat("d", 64)
	RewriteReferences(w, map[string]string{image: movedImage, files: movedFiles})
	task := w.Tasks["test"]
	if task.Environment.Args["TOOL"].Ref != movedImage || task.Mounts[0].Image != movedImage || task.Steps[0].Mounts[0].Ref != movedFiles || *task.Environment.Args["LITERAL"].Value != "registry.test/undeclared:latest" {
		t.Fatal("rewriting lost declared inputs or rewrote literal native arguments")
	}
}

func TestUsesCannotOverrideMountOwnership(t *testing.T) {
	ref := pin("mounted")
	for _, override := range []string{
		"mounts: [{type: cache, target: /cache}]",
		"contexts: {fixtures: ../another}",
	} {
		root := "version: 1\nimports: {tools: " + ref + "}\ntasks:\n  use:\n    uses: tools/test\n    " + override + "\n"
		_, err := loader(map[string]*artifact.Document{ref: {Data: []byte("version: 1\ntasks:\n  test: {image: scratch, steps: [{run: [check]}]}\n")}}).Load(context.Background(), local(t, root))
		if err == nil {
			t.Fatalf("accepted ownership override %s", override)
		}
	}
}

func TestInlineEnvironmentDefinitionFilesUseExistingResolution(t *testing.T) {
	caller, owner := t.TempDir(), t.TempDir()
	for _, directory := range []string{caller, owner} {
		if err := os.WriteFile(filepath.Join(directory, "ci.coopr"), []byte("unchanged native definition"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	task := workflow.Task{Environment: &workflow.CooprBuild{Files: []string{"ci.coopr"}}}
	files, err := DefinitionFilesForTask(task, caller)
	if err != nil || !reflect.DeepEqual(files, []string{filepath.Join(caller, "ci.coopr")}) {
		t.Fatalf("caller environment definitions = %v, %v", files, err)
	}
	task.Environment.FilesFrom = "component"
	if _, err := DefinitionFilesForTask(task, caller); err == nil {
		t.Fatal("unresolved component environment accepted")
	}
	task.ComponentDir = owner
	files, err = DefinitionFilesForTask(task, caller)
	if err != nil || !reflect.DeepEqual(files, []string{filepath.Join(owner, "ci.coopr")}) {
		t.Fatalf("component environment definitions = %v, %v", files, err)
	}
}
