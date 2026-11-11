package workflow

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestMountsAndInlineEnvironmentRoundTrip(t *testing.T) {
	ref := "registry.test/tools/files@sha256:" + strings.Repeat("a", 64)
	w := nativeWorkflow(t, `version: 1
assets: [helpers/tool]
inputs: {version: {required: true}}
tasks:
  base: {image: scratch}
  test:
    environment:
      files: [ci.coopr]
      target: test
      args: {BASE: {from: base}, VERSION: {input: version}}
    contexts: {fixtures: ./fixtures}
    env: {MESSAGE: hello}
    workdir: /workspace
    mounts:
      - {type: context, context: fixtures, source: data, target: /fixtures}
      - {type: context, context: stoke-assets, target: /helpers}
      - {type: cache, id: compiler, sharing: locked, target: /cache}
    steps:
      - run: [check]
        mounts:
          - {type: host, source: ../results, target: /results, read-only: false}
          - {type: image, image: registry.test/tool:v1, source: /bin, target: /tool, read-only: false}
          - {type: stage, from: base, source: /out, target: /built}
          - {type: artifact, ref: "`+ref+`", source: file.tar, target: /file.tar}
`)
	if got := w.Tasks["test"].Dependencies(); !reflect.DeepEqual(got, []string{"base"}) {
		t.Fatalf("mount/environment dependencies = %v", got)
	}
	encoded, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	decoded := nativeWorkflow(t, string(encoded))
	if !reflect.DeepEqual(decoded, w) {
		t.Fatalf("roundtrip changed workflow:\n%s", encoded)
	}
	if err := w.BindInputs(map[string]string{"version": "1.2"}); err != nil {
		t.Fatal(err)
	}
	if got := *w.Tasks["test"].Environment.Args["VERSION"].Value; got != "1.2" {
		t.Fatalf("environment input = %q", got)
	}
	plan, err := w.Expand([]string{"test"})
	if err != nil {
		t.Fatal(err)
	}
	task := plan.Groups[1].Jobs[0].Definition
	if task.Environment == nil || task.OutputKind() != "image" || task.Environment.Args["BASE"].From != "base" {
		t.Fatalf("environment or producer binding lost: %+v", task)
	}
	if !task.Mounts[0].IsReadOnly() || task.Mounts[2].IsReadOnly() || task.Steps[0].Mounts[0].IsReadOnly() || task.Steps[0].Mounts[1].IsReadOnly() || !task.Steps[0].Mounts[3].IsReadOnly() {
		t.Fatal("mount access defaults or explicit overlay/live writes lost")
	}
	*task.Steps[0].Mounts[0].ReadOnly = true
	task.Contexts["fixtures"] = "changed"
	task.Environment.Files[0] = "changed.coopr"
	*task.Environment.Args["VERSION"].Value = "changed"
	original := w.Tasks["test"]
	if *original.Steps[0].Mounts[0].ReadOnly || original.Contexts["fixtures"] != "./fixtures" || original.Environment.Files[0] != "ci.coopr" || *original.Environment.Args["VERSION"].Value != "1.2" {
		t.Fatal("expanded environment/mounts alias workflow source")
	}
}

func TestMountAndEnvironmentSchemaRejectsAmbiguity(t *testing.T) {
	pinned := "registry.test/files@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, task string }{
		{"environment image collision", "image: scratch, environment: {files: [ci.coopr]}"},
		{"environment native collision", "coopr: {files: [ci.coopr]}, environment: {files: [ci.coopr]}"},
		{"component environment", "environment: {files: [ci.coopr], kind: component}"},
		{"missing environment file", "environment: {files: []}"},
		{"escaping environment file", "environment: {files: [../ci.coopr]}"},
		{"environment input missing", "environment: {files: [ci.coopr], args: {VERSION: {input: absent}}}"},
		{"environment dependency missing", "environment: {files: [ci.coopr], args: {BASE: {from: absent}}}"},
		{"unknown mount", "image: scratch, steps: [{run: [check], mounts: [{type: secret, target: /secret}]}]"},
		{"relative target", "image: scratch, steps: [{run: [check], mounts: [{type: cache, target: cache}]}]"},
		{"unnormalized target", "image: scratch, steps: [{run: [check], mounts: [{type: cache, target: /a/../cache}]}]"},
		{"copy step mounts", "image: scratch, steps: [{copy: {sources: [x], destination: /x}, mounts: [{type: cache, target: /cache}]}]"},
		{"native task mounts", "coopr: {files: [ci.coopr]}, mounts: [{type: cache, target: /cache}]"},
		{"no run mounts", "image: scratch, mounts: [{type: cache, target: /cache}]"},
		{"duplicate step mount", "image: scratch, steps: [{run: [check], mounts: [{type: cache, target: /cache}, {type: context, target: /cache}]}]"},
		{"duplicate default mount", "image: scratch, mounts: [{type: cache, target: /cache}], steps: [{run: [check], mounts: [{type: context, target: /cache}]}]"},
		{"unknown context", "image: scratch, steps: [{run: [check], mounts: [{type: context, context: missing, target: /source}]}]"},
		{"unowned component context", "image: scratch, steps: [{run: [check], mounts: [{type: context, context: stoke-assets, target: /source}]}]"},
		{"reserved context", "image: scratch, contexts: {stoke-assets: ./files}"},
		{"empty context", "image: scratch, contexts: {files: ''}"},
		{"context mixed image", "image: scratch, steps: [{run: [check], mounts: [{type: context, image: registry.test/tool:v1, target: /source}]}]"},
		{"context traversal", "image: scratch, steps: [{run: [check], mounts: [{type: context, source: ../outside, target: /source}]}]"},
		{"context absolute", "image: scratch, steps: [{run: [check], mounts: [{type: context, source: /outside, target: /source}]}]"},
		{"host empty", "image: scratch, steps: [{run: [check], mounts: [{type: host, target: /source}]}]"},
		{"host mixed image", "image: scratch, steps: [{run: [check], mounts: [{type: host, source: ./data, image: registry.test/tool:v1, target: /source}]}]"},
		{"host unknown relabel", "image: scratch, steps: [{run: [check], mounts: [{type: host, source: ./data, target: /source, relabel: automatic}]}]"},
		{"input relabel", "image: scratch, steps: [{run: [check], mounts: [{type: context, target: /source, relabel: shared}]}]"},
		{"cache source", "image: scratch, steps: [{run: [check], mounts: [{type: cache, source: ./data, target: /cache}]}]"},
		{"cache invalid sharing", "image: scratch, steps: [{run: [check], mounts: [{type: cache, sharing: exclusive, target: /cache}]}]"},
		{"cache unsupported private sharing", "image: scratch, steps: [{run: [check], mounts: [{type: cache, sharing: private, target: /cache}]}]"},
		{"id on content", "image: scratch, steps: [{run: [check], mounts: [{type: context, id: files, target: /source}]}]"},
		{"stage missing", "image: scratch, steps: [{run: [check], mounts: [{type: stage, from: absent, target: /source}]}]"},
		{"image empty", "image: scratch, steps: [{run: [check], mounts: [{type: image, target: /source}]}]"},
		{"artifact tag", "image: scratch, steps: [{run: [check], mounts: [{type: artifact, ref: registry.test/files:latest, target: /source}]}]"},
		{"artifact writable", "image: scratch, steps: [{run: [check], mounts: [{type: artifact, ref: '" + pinned + "', target: /source, read-only: false}]}]"},
		{"artifact traversal", "image: scratch, steps: [{run: [check], mounts: [{type: artifact, ref: '" + pinned + "', source: ../file, target: /source}]}]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader("version: 1\ntasks:\n  task: {" + tc.task + "}\n")); err == nil {
				t.Fatal("accepted invalid mount or environment")
			}
		})
	}
}

func TestMountSourcesRejectComponentsAndMatrixAmbiguity(t *testing.T) {
	for _, source := range []string{
		"coopr: {kind: component, files: [tool.coopr]}",
		"publish: {from: base, destinations: [registry.test/output:v1]}",
	} {
		doc := "version: 1\ntasks:\n  base: {image: scratch}\n  producer: {" + source + "}\n  task: {image: scratch, steps: [{run: [check], mounts: [{type: stage, from: producer, target: /source}]}]}\n"
		if _, err := Decode(strings.NewReader(doc)); err == nil {
			t.Fatal("accepted nonimage mount source")
		}
	}
	w := nativeWorkflow(t, `version: 1
tasks:
  producer:
    image: scratch
    strategy: {matrix: {mode: [a, b]}}
  task:
    image: scratch
    steps: [{run: [check], mounts: [{type: stage, from: producer, target: /source}]}]
`)
	if _, err := w.Expand([]string{"task"}); err == nil || !strings.Contains(err.Error(), "multiple matrix variants") {
		t.Fatalf("ambiguous stage mount accepted: %v", err)
	}
}

func TestEnvironmentMatrixArgumentsAndOrdinaryEnvironment(t *testing.T) {
	w := nativeWorkflow(t, `version: 1
tasks:
  base: {image: scratch}
  task:
    environment: {files: [ci.coopr], args: {BASE: {from: base}, MODE: {matrix: mode}}}
    strategy: {matrix: {mode: [debug, release]}}
    steps: [{run: [check]}]
`)
	plan, err := w.Expand([]string{"task"})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range plan.Groups[1].Jobs {
		if got := *job.Definition.Environment.Args["MODE"].Value; got != job.Matrix["mode"] {
			t.Fatalf("environment matrix argument = %q, want %q", got, job.Matrix["mode"])
		}
		if got := *job.Definition.Env["STOKE_MATRIX_MODE"].Value; got != job.Matrix["mode"] {
			t.Fatalf("ordinary matrix environment = %q, want %q", got, job.Matrix["mode"])
		}
	}
}
