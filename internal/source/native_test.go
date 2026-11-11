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

func TestImportedNativeReferencesAndOutputs(t *testing.T) {
	ref := pin("native")
	destination := "destination.test/images:release"
	literal := "source.test/components:literal"
	library := `version: 1
tasks:
  base:
    image: scratch
  toolkit:
    coopr:
      kind: component
      files: [definitions/toolkit.coopr]
  image:
    coopr:
      files: [definitions/image.coopr]
      target: release
      args:
        BASE: {from: base}
        TOOLKIT: {from: toolkit}
        VERSION: "` + literal + `"
    outputs: {filesystem: true}
  verify:
    from: image
  deliver:
    publish:
      from: image
      destinations: ["` + destination + `"]
    needs: [verify]
`
	path := local(t, "version: 1\nimports: {native: "+ref+"}\ntasks:\n  build:\n    uses: native/image\n    outputs: {filesystem: false}\n  sibling:\n    uses: native/image\n  release:\n    uses: native/deliver\n  otherRelease:\n    uses: native/deliver\n")
	result, err := loader(map[string]*artifact.Document{ref: {
		Data: []byte(library), Dependencies: map[string]string{literal: "moved.test/component:v1", destination: "moved.test/output:v1"},
	}}).Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	build := result.Workflow.Tasks["build"]
	if build.Coopr.Args["BASE"].From != "build--native--base" || build.Coopr.Args["TOOLKIT"].From != "build--native--toolkit" {
		t.Fatalf("native task inputs did not retain import namespace: %#v", build.Coopr.Args)
	}
	if !reflect.DeepEqual(build.Coopr.Files, []string{"definitions/image.coopr"}) || build.Coopr.Target != "release" || *build.Coopr.Args["VERSION"].Value != literal {
		t.Fatalf("native caller file, target or literal argument changed: %#v", build.Coopr)
	}
	if build.Outputs.Filesystem == nil || *build.Outputs.Filesystem {
		t.Fatalf("local output override lost: %#v", build.Outputs)
	}
	release := result.Workflow.Tasks["release"]
	if release.Publish.From != "release--native--image" || !reflect.DeepEqual(release.Needs, []string{"release--native--verify"}) || !reflect.DeepEqual(release.Publish.Destinations, []string{destination}) {
		t.Fatalf("publication source/gate/destination changed: %#v", release)
	}
	if result.ContextDir != filepath.Dir(path) {
		t.Fatalf("import lost caller context: %q", result.ContextDir)
	}
	// Mutating one expanded use must not mutate its reusable source or siblings.
	build.Coopr.Files[0] = "changed.coopr"
	*build.Coopr.Args["VERSION"].Value = "changed"
	argument := build.Coopr.Args["BASE"]
	argument.From = "changed"
	build.Coopr.Args["BASE"] = argument
	*result.Workflow.Tasks["sibling"].Outputs.Filesystem = false
	release.Publish.Destinations[0] = "changed.test/output:v1"
	for _, name := range []string{"sibling"} {
		task := result.Workflow.Tasks[name]
		if task.Coopr.Files[0] != "definitions/image.coopr" || *task.Coopr.Args["VERSION"].Value != literal || task.Coopr.Args["BASE"].From != "sibling--native--base" {
			t.Fatalf("native template aliases task %q: %#v", name, task.Coopr)
		}
	}
	if !*result.Workflow.Tasks["release--native--image"].Outputs.Filesystem {
		t.Fatal("inherited outputs alias their source template")
	}
	for _, name := range []string{"otherRelease"} {
		if got := result.Workflow.Tasks[name].Publish.Destinations[0]; got != destination {
			t.Fatalf("publication destinations alias task %q: %q", name, got)
		}
	}
}

func TestNativeInputsAndPublicationDoNotBecomeRegistryDependencies(t *testing.T) {
	data := `version: 1
imports: {shared: ` + pin("shared") + `}
tasks:
  native:
    coopr:
      files: [definition.coopr]
      args: {COMPONENT: "registry.test/component:v1"}
  publish:
    publish:
      from: native
      destinations: [registry.test/output:v1]
`
	w, err := workflow.Decode(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := Dependencies(w); !reflect.DeepEqual(got, []string{pin("shared")}) {
		t.Fatalf("undeclared native literals or output destinations became inputs: %v", got)
	}
}

func TestNativeTemplateOverridesRetainTaskKindValidation(t *testing.T) {
	ref := pin("native")
	library := []byte(`version: 1
tasks:
  component:
    coopr: {kind: component, files: [component.coopr]}
  image:
    coopr: {files: [image.coopr]}
  publish:
    publish: {from: image, destinations: [registry.test/output:v1]}
`)
	for _, tc := range []struct {
		name, task, override string
	}{
		{"native-env", "image", "env: {MODE: ignored}"},
		{"native-workdir", "image", "workdir: /ignored"},
		{"component-filesystem", "component", "outputs: {filesystem: true}"},
		{"publication-cache", "publish", "cache: false"},
		{"publication-outputs", "publish", "outputs: {filesystem: false}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := local(t, "version: 1\nimports: {native: "+ref+"}\ntasks:\n  use:\n    uses: native/"+tc.task+"\n    "+tc.override+"\n")
			if _, err := loader(map[string]*artifact.Document{ref: {Data: library}}).Load(context.Background(), path); err == nil {
				t.Fatal("accepted incompatible override after template expansion")
			}
		})
	}
}

func TestDefinitionFilesResolveCallerContext(t *testing.T) {
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(contextDir, "white space ☃.coopr")
	second := filepath.Join(contextDir, "second.coopr")
	for _, file := range []string{first, second} {
		if err := os.WriteFile(file, []byte("not parsed by Stoke"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	contextAlias := filepath.Join(root, "context-alias")
	if err := os.Symlink(contextDir, contextAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("white space ☃.coopr", filepath.Join(contextDir, "inside.coopr")); err != nil {
		t.Fatal(err)
	}
	got, err := DefinitionFiles(contextAlias, []string{"second.coopr", "inside.coopr"})
	if err != nil || !reflect.DeepEqual(got, []string{second, first}) {
		t.Fatalf("canonical ordered files=%v error=%v", got, err)
	}
	data, err := os.ReadFile(first)
	if err != nil || string(data) != "not parsed by Stoke" {
		t.Fatalf("definition input changed: %q %v", data, err)
	}
	outside := filepath.Join(root, "outside.coopr")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(contextDir, "escape.coopr")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(contextDir, "directory.coopr"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		context string
		files   []string
	}{
		{"empty", contextDir, nil},
		{"empty-name", contextDir, []string{""}},
		{"absolute", contextDir, []string{first}},
		{"parent", contextDir, []string{"../outside.coopr"}},
		{"symlink-escape", contextDir, []string{"escape.coopr"}},
		{"missing", contextDir, []string{"missing.coopr"}},
		{"directory", contextDir, []string{"directory.coopr"}},
		{"duplicate", contextDir, []string{"white space ☃.coopr", "inside.coopr"}},
		{"bad-context", first, []string{"second.coopr"}},
		{"missing-context", filepath.Join(root, "missing"), []string{"second.coopr"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := DefinitionFiles(tc.context, tc.files); err == nil {
				t.Fatalf("invalid definition inputs accepted: %v", got)
			}
		})
	}
}
