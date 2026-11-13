package workflow

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestPackagedWorkflowSchema(t *testing.T) {
	ref := "registry.test/components/tool@sha256:" + strings.Repeat("a", 64)
	document := `version: 1
assets: [build.coopr, scripts/check.sh]
tasks:
  build:
    coopr:
      files: [build.coopr]
      files-from: component
      args: {TOOL: {ref: "` + ref + `"}}
  test:
    from: build
    steps:
      - copy: {scope: component, sources: [scripts/check.sh], destination: /check.sh}
      - export: {source: /dist/tool, destination: dist/tool}
`
	w, err := Decode(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	build := w.Tasks["build"]
	build.ComponentDir = "/private/package"
	w.Tasks["build"] = build
	test := w.Tasks["test"]
	test.ComponentDir = build.ComponentDir
	w.Tasks["test"] = test
	encoded, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "/private/package") {
		t.Fatal("runtime package directory leaked into published YAML")
	}
	reloaded, err := Decode(strings.NewReader(string(encoded)))
	if err != nil || reloaded.Tasks["build"].Coopr.Args["TOOL"].Ref != ref || reloaded.Tasks["test"].Steps[0].Copy.Scope != "component" {
		t.Fatalf("package roundtrip err=%v doc=%s", err, encoded)
	}
	plan, err := w.Expand([]string{"test"})
	if err != nil || plan.Groups[0].Jobs[0].Definition.ComponentDir != "/private/package" {
		t.Fatalf("owner lost in expansion: %v %v", plan, err)
	}
	if got := plan.Groups[1].Jobs[0].Definition.FileExports()[0]; got.Source != "/dist/tool" || got.Destination != "dist/tool" || reloaded.Tasks["test"].FileExports()[0] != got || plan.Groups[1].Jobs[0].Definition.ComponentDir != "/private/package" {
		t.Fatalf("package ownership changed caller export paths: %#v", got)
	}
}

func TestPackagedWorkflowRejectsAmbiguousInputs(t *testing.T) {
	for _, document := range []string{
		"version: 1\nassets: [../outside]\ntasks: {a: {image: scratch}}",
		"version: 1\nassets: [./file]\ntasks: {a: {image: scratch}}",
		"version: 1\nassets: [stoke.yaml]\ntasks: {a: {image: scratch}}",
		"version: 1\nassets: [file, file]\ntasks: {a: {image: scratch}}",
		"version: 1\nassets: [file, file/child]\ntasks: {a: {image: scratch}}",
		"version: 1\nassets: [!!binary Zv8=]\ntasks: {a: {image: scratch}}",
		"version: 1\ntasks: {a: {image: scratch, steps: [{copy: {scope: other, sources: [file], destination: /file}}]}}",
		"version: 1\ntasks: {a: {image: scratch}, b: {image: scratch, steps: [{copy: {from: a, scope: context, sources: [/file], destination: /file}}]}}",
		"version: 1\ntasks: {a: {image: scratch, steps: [{copy: {scope: component, sources: [file], destination: /file}}]}}",
		"version: 1\ntasks: {a: {coopr: {files: [build.coopr], files-from: component}}}",
		"version: 1\ntasks: {a: {coopr: {files: [build.coopr], files-from: other}}}",
		"version: 1\ntasks: {a: {coopr: {files: [build.coopr], args: {TOOL: {ref: 'registry.test/tool:latest'}}}}}",
		"version: 1\ntasks: {a: {coopr: {files: [build.coopr], args: {TOOL: {ref: 'oci://registry.test/tool@sha256:" + strings.Repeat("a", 64) + "'}}}}}",
		"version: 1\ntasks: {a: {coopr: {files: [build.coopr], args: {TOOL: {ref: 'team/tool@sha256:" + strings.Repeat("a", 64) + "'}}}}}",
		"version: 1\ntasks: {a: {coopr: {files: [build.coopr], args: {TOOL: {ref: './local@sha256:" + strings.Repeat("a", 64) + "'}}}}}",
		"version: 1\ntasks: {a: {coopr: {files: [build.coopr], args: {TOOL: {ref: '../local@sha256:" + strings.Repeat("a", 64) + "'}}}}}",
	} {
		if _, err := Decode(strings.NewReader(document)); err == nil {
			t.Fatalf("accepted ambiguous package input: %s", document)
		}
	}
}
